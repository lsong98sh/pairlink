// Package logx 封装 log/slog，为三种模式提供统一的日志构造方式。
//
// 不自定义日志格式：text / json 由 slog 原生 handler 输出，级别与字段遵循
// docs/architecture.md 中的字段约定（mode、agent_id、client_id、service_id、
// pairing_key、remote、event、err）。
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Config 是日志构造参数。
type Config struct {
	// Level 是日志级别：debug / info / warn / error；空值表示 info。
	Level string
	// Format 是输出格式：text / json；空值表示 text。
	Format string
	// Output 是输出目标；nil 表示 DefaultOutput()（默认 os.Stderr）。
	Output io.Writer
	// Mode 是模式名（relay / agent / client），非空时写入每条日志的 mode 字段。
	Mode string
	// Component 是组件名，非空时写入每条日志的 component 字段。
	Component string
	// KeepDays 是 NewFileLogger 的日文件保留天数：nil 表示默认
	// DefaultKeepDays（7）天；0 表示永久保留；负数非法（构造时报错）。
	KeepDays *int
	// now 是时钟注入点（仅包内可见），nil 表示 time.Now；单测用它模拟跨天。
	now func() time.Time
}

// defaultOutput 是 Config.Output 为 nil 时 logx.New 使用的输出目标。
var (
	defaultOutMu sync.RWMutex
	defaultOut   io.Writer
)

// SetDefaultOutput 设置 Config.Output 为空（nil）时 logx.New 使用的输出目标；
// w 为 nil 时恢复 os.Stderr。
//
// main 在 Windows 服务模式下用它把各包内 logx.New 产生的日志一并写入按日期
// 切分的日志文件——这样跨天切换对所有 logger 都生效，而不需要把
// os.Stderr 指向某一天的固定文件。
func SetDefaultOutput(w io.Writer) {
	defaultOutMu.Lock()
	defaultOut = w
	defaultOutMu.Unlock()
}

// DefaultOutput 返回当前的默认输出目标（未设置时为 os.Stderr）。
func DefaultOutput() io.Writer {
	defaultOutMu.RLock()
	w := defaultOut
	defaultOutMu.RUnlock()
	if w == nil {
		return os.Stderr
	}
	return w
}

// ParseLevel 把字符串解析为 slog.Level（大小写不敏感）。
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("未知日志级别 %q（可选 debug / info / warn / error）", s)
	}
}

// New 按 cfg 构造 *slog.Logger。
//
// 未指定级别/格式时使用 info/text；Output 为 nil 时写 DefaultOutput()
// （默认 os.Stderr，Windows 服务模式下由 main 指向日志文件）。
// 返回值可直接交给 slog 包使用，也可用 Component 派生组件 logger。
func New(cfg Config) *slog.Logger {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		level = slog.LevelInfo
	}
	out := cfg.Output
	if out == nil {
		out = DefaultOutput()
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "json":
		handler = slog.NewJSONHandler(out, opts)
	default:
		handler = slog.NewTextHandler(out, opts)
	}

	log := slog.New(handler)
	attrs := make([]any, 0, 4)
	if cfg.Mode != "" {
		attrs = append(attrs, slog.String("mode", cfg.Mode))
	}
	if cfg.Component != "" {
		attrs = append(attrs, slog.String("component", cfg.Component))
	}
	if len(attrs) > 0 {
		log = log.With(attrs...)
	}
	return log
}

// Component 返回带 component 字段的派生 logger，用于区分同一进程内的子模块。
func Component(log *slog.Logger, name string) *slog.Logger {
	if log == nil {
		log = slog.Default()
	}
	if name == "" {
		return log
	}
	return log.With(slog.String("component", name))
}

// MaxAttrLen 是日志中来自对端（不可信）的字符串字段的最大长度（按 rune 计）。
//
// SEC-M2：client_name / agent_id / 错误信息 / 踢出原因等字段由对端控制，
// 先在调用点用 CleanN(s, MaxAttrLen) 清洗，避免：
//   - 1 MiB client_name 让每条日志放大成 1 MiB（日志放大/磁盘打满）；
//   - 值里的 \n / \r 伪造额外日志行（日志注入）。
const MaxAttrLen = 64

// CleanN 把外部字符串清洗为可安全写入单行日志的形式：
//   - 去掉全部控制字符（含 \n \r \t \0）；
//   - 非法 UTF-8 字节替换为 '?'；
//   - 按 rune 截断到最多 max 个字符，被截断时追加 '…'。
//
// max <= 0 时只做清洗、不截断。
func CleanN(s string, max int) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "?")
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if max > 0 && count >= max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// NewFileLogger 构造按**本地日期**切分写入日志文件的 logger。
//
// path 视为**基名**，实际写入 <base>-YYYY-MM-DD<ext>：
//
//	/var/log/pairlink/relay.log → /var/log/pairlink/relay-2026-11-15.log
//	C:\Logs\relay              → C:\Logs\relay-2026-11-15（无扩展名）
//
// 目录不存在时自动创建。写入时发现本地日期变化就关闭旧文件、打开新文件，
// 并清理过期日文件（保留天数见 Config.KeepDays：nil = 默认 7 天，0 = 永久
// 保留）。清理只删除同目录、同基名且严格匹配 <base>-YYYY-MM-DD[.ext] 的文件，
// 删除失败（Windows 文件被占用等）只记 WARN，不影响主流程。
//
// path 为空时退回 logx.New 的默认输出（默认 os.Stderr，等价于前台模式），
// 此时返回的 Closer 是空操作。返回的 io.Closer 需要由调用方在进程退出前
// 关闭（可重复调用）。
func NewFileLogger(path string, cfg Config) (*slog.Logger, io.Closer, error) {
	if strings.TrimSpace(path) == "" {
		return New(cfg), nopCloser{}, nil
	}
	days, err := keepDays(cfg)
	if err != nil {
		return nil, nil, err
	}
	w, fails, err := newDateWriter(path, days, cfg.now)
	if err != nil {
		return nil, nil, err
	}
	cfg.Output = w
	log := New(cfg)
	// 清理失败的 WARN 用直达文件的专用 logger：slog handler 在 Write 期间
	// 持有自己的互斥锁，在 Write 里回调同一个 logger 会死锁。
	w.log.Store(newWarnLogger(w, cfg.Format, cfg.Mode))
	// 启动清理的失败项：logger 已就绪，走统一的 WARN 出口。
	w.warn(fails)
	return log, w, nil
}
