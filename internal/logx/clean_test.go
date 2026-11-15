package logx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCleanN 覆盖 SEC-M2 的日志字段清洗规则。
func TestCleanN(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"空串", "", 64, ""},
		{"普通文本", "hello", 64, "hello"},
		{"去换行与制表", "a\nb\r\tc\x00d", 64, "abcd"},
		{"去其它控制字符", "x\x1b[31my\x07", 64, "x[31my"},
		{"非法UTF8替换", "a\xffb", 64, "a?b"},
		{"不截断时长度不变", strings.Repeat("A", 10), 64, strings.Repeat("A", 10)},
		{"max=0只清洗不截断", "a\nb", 0, "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanN(tc.in, tc.max); got != tc.want {
				t.Fatalf("CleanN(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}

	// 按 rune 截断（不能把多字节字符截成半个），被截断时追加省略号。
	got := CleanN(strings.Repeat("中", 100), 64)
	if n := utf8.RuneCountInString(got); n != MaxAttrLen+1 {
		t.Fatalf("截断后 rune 数 = %d, want %d（64 字符 + 省略号）", n, MaxAttrLen+1)
	}
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("截断结果应合法且以省略号结尾: %q", got)
	}
}

// TestCleanNKeepsLogLineSmall 是 SEC-M2 的直接回归：1 MiB 的 client_name
// （以及其中的换行注入）经 CleanN 后写入日志，整行 < 512 字节且只有一行。
func TestCleanNKeepsLogLineSmall(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	evil := strings.Repeat("A", 1<<20) + "\nlevel=ERROR msg=INJECTED"
	log.Info("控制面接入成功", "event", "hello_ok",
		"client_name", CleanN(evil, MaxAttrLen))

	line := buf.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Fatalf("清洗后应只有一行日志，实际 %d 行:\n%s", n, line)
	}
	if len(line) >= 512 {
		t.Fatalf("单行日志长度 = %d 字节，应 < 512（清洗前 %d 字节）", len(line), len(evil))
	}
	if strings.Contains(line, "INJECTED") || strings.Contains(line, "level=ERROR") {
		t.Fatalf("注入内容不应出现在日志行中: %s", line)
	}
	t.Logf("1 MiB client_name 清洗后日志行 %d 字节", len(line))
}
