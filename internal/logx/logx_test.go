package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewText(t *testing.T) {
	var buf bytes.Buffer
	log := New(Config{Level: "debug", Format: "text", Output: &buf, Mode: "relay"})
	log.Debug("hello", "service_id", "default")
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("text 输出缺少级别: %q", out)
	}
	if !strings.Contains(out, "mode=relay") {
		t.Errorf("text 输出缺少 mode 字段: %q", out)
	}
	if !strings.Contains(out, "service_id=default") {
		t.Errorf("text 输出缺少业务字段: %q", out)
	}
}

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	log := New(Config{Level: "info", Format: "json", Output: &buf, Mode: "agent"})
	log.Info("registered", "service_id", "web")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("json 输出不可解析: %v (%q)", err, buf.String())
	}
	if m["msg"] != "registered" || m["mode"] != "agent" || m["service_id"] != "web" {
		t.Errorf("json 字段错误: %+v", m)
	}
}

func TestComponent(t *testing.T) {
	var buf bytes.Buffer
	base := New(Config{Output: &buf, Format: "text"})
	log := Component(base, "tunnel")
	log.Info("x")
	if !strings.Contains(buf.String(), "component=tunnel") {
		t.Errorf("Component 未附加 component 字段: %q", buf.String())
	}
	if Component(nil, "x") == nil {
		t.Error("Component(nil) 不应返回 nil")
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range tests {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q) 失败: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("ParseLevel(trace) 应报错")
	}
}

func TestNewFileLogger(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "logs", "pairlink-relay.log")
	day := time.Date(2026, 11, 15, 9, 30, 0, 0, time.Local)
	log, closer, err := NewFileLogger(base, Config{
		Level: "info", Format: "text", Mode: "relay",
		now: func() time.Time { return day },
	})
	if err != nil {
		t.Fatalf("NewFileLogger 失败: %v", err)
	}
	log.Info("file log", "event", "start")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志文件失败: %v", err)
	}
	path := filepath.Join(dir, "logs", "pairlink-relay-2026-11-15.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if !strings.Contains(string(data), "file log") || !strings.Contains(string(data), "mode=relay") {
		t.Errorf("日志文件内容错误: %q", string(data))
	}
}
