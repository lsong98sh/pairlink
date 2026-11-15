package logx

// 本文件实现按**本地日期**切分的日志文件写入与过期日文件清理：
// 文件名 <base>-YYYY-MM-DD<ext>，写入时发现日期变化就关闭旧文件、打开新文件。
// 不做体积切割，也不引入第三方依赖（只覆盖"按天 + 保留 N 天"这一个诉求，
// 单文件体积上限仍交给 logrotate / 磁盘配额）。

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// dateLayout 是日志文件名中的日期格式，也是清理时的解析格式。
	dateLayout = "2006-01-02"

	// DefaultKeepDays 是 [common] log_keep_days 的默认值（保留当天与前 7 天）。
	DefaultKeepDays = 7
)

// ValidateKeepDays 校验日文件保留天数：0 表示永久保留，负数非法。
//
// 该函数供配置层（[common] log_keep_days）复用，日志包自身也在
// NewFileLogger 里再校验一次，避免调用方绕过配置层。
func ValidateKeepDays(days int) error {
	if days < 0 {
		return fmt.Errorf("log_keep_days 不能为负数（当前 %d；0 表示永久保留，默认 %d 天）",
			days, DefaultKeepDays)
	}
	return nil
}

// keepDays 解析 cfg.KeepDays：nil 表示默认 DefaultKeepDays。
func keepDays(cfg Config) (int, error) {
	days := DefaultKeepDays
	if cfg.KeepDays != nil {
		days = *cfg.KeepDays
	}
	if err := ValidateKeepDays(days); err != nil {
		return 0, err
	}
	return days, nil
}

// splitLogPath 把 log_path 拆成目录、基名主干与扩展名。
//
//	log_path = /var/log/pairlink/relay.log → dir=/var/log/pairlink, stem=relay, ext=.log
//	log_path = C:\Logs\relay             → dir=C:\Logs,          stem=relay, ext=""
func splitLogPath(path string) (dir, stem, ext string, err error) {
	path = filepath.Clean(path)
	dir = filepath.Dir(path)
	base := filepath.Base(path)
	ext = filepath.Ext(base)
	stem = strings.TrimSuffix(base, ext)
	if stem == "" || stem == "." || stem == string(filepath.Separator) || dir == "" {
		return "", "", "", fmt.Errorf("日志文件路径 %q 非法：缺少基名", path)
	}
	return dir, stem, ext, nil
}

// datedName 返回某一天的日志文件名：<stem>-YYYY-MM-DD<ext>。
func datedName(stem, ext string, day time.Time) string {
	return stem + "-" + day.Format(dateLayout) + ext
}

// datedPath 返回某一天的日志文件完整路径。
func datedPath(dir, stem, ext string, day time.Time) string {
	return filepath.Join(dir, datedName(stem, ext, day))
}

// DatedPath 返回 base 在 day 当天对应的日文件路径，命名规则与写入器一致。
//
// 供服务模式把裸 stderr（panic 栈、flag 解析错误等非 slog 输出）也落到当天文件；
// 日期变化后 stderr 仍指向启动当天的文件，这是有意的取舍（见 docs/architecture.md §7.5）。
func DatedPath(base string, day time.Time) (string, error) {
	dir, stem, ext, err := splitLogPath(base)
	if err != nil {
		return "", err
	}
	return datedPath(dir, stem, ext, day), nil
}

// parseDatedName 判断 name 是否**严格**匹配 <stem>-YYYY-MM-DD<ext>。
//
// "严格"指：基名主干逐字节相同 + 日期必须是 10 位补零格式 + 日期真实存在
// （2026-02-30、2026-1-01、2026-13-01 一律不匹配）。不匹配时 ok=false，
// 调用方据此**绝不**删除该文件。
func parseDatedName(name, stem, ext string) (time.Time, bool) {
	if stem == "" || !strings.HasPrefix(name, stem+"-") {
		return time.Time{}, false
	}
	rest := name[len(stem)+1:]
	if !strings.HasSuffix(rest, ext) {
		return time.Time{}, false
	}
	mid := rest[:len(rest)-len(ext)]
	if len(mid) != len(dateLayout) {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation(dateLayout, mid, time.Local)
	if err != nil || day.Format(dateLayout) != mid {
		return time.Time{}, false
	}
	return day, true
}

// cleanupFailure 是一次删除过期日文件失败的记录（只用于 WARN）。
type cleanupFailure struct {
	path string
	err  error
}

// dateWriter 是按日期切分写入的日志文件句柄。
//
// 所有文件操作都在 mu 下串行，因此可被多个 goroutine（多个 slog handler）
// 并发写；跨天切换期间写入方阻塞在锁上，不会丢行也不会 panic。
type dateWriter struct {
	mu       sync.Mutex
	dir      string
	stem     string
	ext      string
	curPath  string // 当前打开的日文件完整路径；未打开时为空
	keepDays int
	now      func() time.Time
	f        *os.File
	day      string // 当前打开文件对应的本地日期 YYYY-MM-DD
	closed   bool
	log      atomic.Pointer[slog.Logger] // 清理失败时的 WARN 出口（构造后设置）
	// remove 删除过期日文件；单测注入失败以模拟 Windows 文件被占用。
	remove func(string) error
}

// newDateWriter 打开当天的日志文件并清理过期日文件，返回清理失败项。
func newDateWriter(path string, days int, now func() time.Time) (*dateWriter, []cleanupFailure, error) {
	dir, stem, ext, err := splitLogPath(path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建日志目录 %q: %w", dir, err)
	}
	if now == nil {
		now = time.Now
	}
	w := &dateWriter{dir: dir, stem: stem, ext: ext, keepDays: days, now: now, remove: os.Remove}
	fails, err := w.ensureDayLocked(now())
	if err != nil {
		return nil, nil, err
	}
	return w, fails, nil
}

// Write 实现 io.Writer：写前检查日期，必要时切换文件，然后整体写入本次载荷。
//
// 单次 Write 对应 slog 的一条完整记录（slog text/json handler 对每条记录只
// 调用一次 Write），因此并发写入不会把两条记录揉成一行。
func (w *dateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, fmt.Errorf("写入日志文件: %w", os.ErrClosed)
	}
	fails, err := w.ensureDayLocked(w.now())
	var n int
	if err == nil {
		n, err = w.f.Write(p)
	}
	w.mu.Unlock()
	// 清理失败的 WARN 在锁外发出；它直达当前文件，不会重入调用方 handler。
	w.warn(fails)
	return n, err
}

// Close 关闭当前日志文件，可重复调用。
func (w *dateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	w.curPath = ""
	if err != nil {
		return fmt.Errorf("关闭日志文件: %w", err)
	}
	return nil
}

// ensureDayLocked 保证当前打开的文件对应 now 所在的本地日期，日期变化时切换
// 文件并清理过期日文件。调用方必须持有 w.mu。
func (w *dateWriter) ensureDayLocked(now time.Time) ([]cleanupFailure, error) {
	day := now.Format(dateLayout)
	if w.f != nil && w.day == day {
		return nil, nil
	}
	w.closeFileLocked()
	full := datedPath(w.dir, w.stem, w.ext, now)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件 %q: %w", full, err)
	}
	w.f = f
	w.day = day
	w.curPath = full
	return w.cleanOldLocked(now), nil
}

// closeFileLocked 关闭当前文件并清空状态；关闭失败无法补救（fd 由运行时回收），
// 不影响随后的切换。调用方必须持有 w.mu。
func (w *dateWriter) closeFileLocked() {
	if w.f == nil {
		return
	}
	_ = w.f.Close()
	w.f = nil
	w.curPath = ""
	w.day = ""
}

// cleanOldLocked 删除同目录、同基名、日期早于 today-keepDays 的日文件。
//
// 只删严格匹配 <stem>-YYYY-MM-DD<ext> 的文件：其它基名（other-2026-01-01.log）、
// 无日期的原始文件（relay.log）、日期格式非法、同名目录一律不动。keepDays <= 0
// 表示永久保留，直接跳过。删除失败只收集为 WARN，不影响写入。调用方必须持有 w.mu。
func (w *dateWriter) cleanOldLocked(today time.Time) []cleanupFailure {
	if w.keepDays <= 0 {
		return nil
	}
	// 用 UTC 做日期算术：本地时区夏令时可能把"本地零点减 N 天"推到前一天；
	// 两个 YYYY-MM-DD 串都是定长补零格式，字典序即时间序，比较无需再解析。
	cutoff := time.Date(today.Year(), today.Month(), today.Day()-w.keepDays,
		0, 0, 0, 0, time.UTC).Format(dateLayout)

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return []cleanupFailure{{path: w.dir, err: err}}
	}
	remove := w.remove
	if remove == nil {
		remove = os.Remove
	}
	var fails []cleanupFailure
	for _, e := range entries {
		if e.IsDir() {
			continue // 同名目录不是日志文件，不删
		}
		day, ok := parseDatedName(e.Name(), w.stem, w.ext)
		if !ok {
			continue
		}
		if day.Format(dateLayout) >= cutoff {
			continue
		}
		full := filepath.Join(w.dir, e.Name())
		if w.curPath != "" && full == w.curPath {
			continue // 绝不删当前正在写的文件
		}
		if err := remove(full); err != nil {
			fails = append(fails, cleanupFailure{path: full, err: err})
		}
	}
	return fails
}

// writeRaw 把一段已格式化的日志（清理失败 WARN）直接追加到当前文件。
//
// 它**不**经过 dateWriter.Write，因此不会重新触发日期检查/清理，也就不会
// 递归；同时它绕开了调用方的 slog handler —— handler 在 Write 期间持有自己的
// 互斥锁，在 Write 里回调同一个 logger 会死锁（sync.Mutex 不可重入）。
func (w *dateWriter) writeRaw(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil // 已关闭：丢弃，不影响主流程
	}
	if _, err := w.f.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// warnSink 是清理失败 WARN 的直达出口：写入当前日志文件。
type warnSink struct{ w *dateWriter }

// Write 实现 io.Writer。
func (s warnSink) Write(p []byte) (int, error) { return s.w.writeRaw(p) }

// newWarnLogger 构造专用于"清理失败"的 logger：格式跟随 cfg，但输出直达日志
// 文件（不经过调用方 handler），因此可以在 Write 的调用栈里安全使用。
func newWarnLogger(w *dateWriter, format, mode string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelWarn}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		h = slog.NewJSONHandler(warnSink{w}, opts)
	} else {
		h = slog.NewTextHandler(warnSink{w}, opts)
	}
	log := slog.New(h)
	if mode != "" {
		log = log.With(slog.String("mode", mode))
	}
	return log
}

// warn 把清理失败按 WARN 记录（logger 尚未就绪时退回 stderr）。
//
// 调用方必须**不**持有 w.mu：WARN 会再写一次日志并重新进入 writeRaw。
func (w *dateWriter) warn(fails []cleanupFailure) {
	for _, fail := range fails {
		if log := w.log.Load(); log != nil {
			log.Warn("日志清理失败，保留该文件",
				"event", "log_cleanup_fail",
				"file", fail.path,
				"err", fail.err)
			continue
		}
		fmt.Fprintf(os.Stderr, "WARN 日志清理失败，保留该文件: %s: %v\n", fail.path, fail.err)
	}
}

// nopCloser 用于 log_path 为空（写 stderr）时的返回值。
type nopCloser struct{}

// Close 总是成功。
func (nopCloser) Close() error { return nil }
