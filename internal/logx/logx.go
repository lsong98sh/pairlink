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
	"path/filepath"
	"strings"
	"unicode"
)

// Config 是日志构造参数。
type Config struct {
	// Level 是日志级别：debug / info / warn / error；空值表示 info。
	Level string
	// Format 是输出格式：text / json；空值表示 text。
	Format string
	// Output 是输出目标；nil 表示 os.Stderr。
	Output io.Writer
	// Mode 是模式名（relay / agent / client），非空时写入每条日志的 mode 字段。
	Mode string
	// Component 是组件名，非空时写入每条日志的 component 字段。
	Component string
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
// 未指定级别/格式时使用 info/text；Output 为 nil 时写 stderr。
// 返回值可直接交给 slog 包使用，也可用 Component 派生组件 logger。
func New(cfg Config) *slog.Logger {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		level = slog.LevelInfo
	}
	out := cfg.Output
	if out == nil {
		out = os.Stderr
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

// NewFileLogger 构造写入 path 的 logger（追加模式，目录不存在时自动创建）。
//
// Windows 服务没有控制台，-service 模式下应使用本函数把日志落到文件
// （约定路径为 exe 同目录的 pairlink-<mode>.log）。返回的 io.Closer 需要由
// 调用方在进程退出前关闭。
func NewFileLogger(path string, cfg Config) (*slog.Logger, io.Closer, error) {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建日志目录 %q: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("打开日志文件 %q: %w", path, err)
	}
	cfg.Output = f
	return New(cfg), f, nil
}
