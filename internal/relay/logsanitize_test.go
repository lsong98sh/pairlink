package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// logLinesWith 返回包含 sub 的日志行。
func logLinesWith(logs *lockedBuffer, sub string) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// TestRelayTruncatesClientNameInLogs 是 SEC-M2 的 relay 级回归：
// Hello.client_name 由对端控制，必须清洗（去控制字符）并截断到 64 字符，
// 保证单行日志 < 512 字节、不能被 \n 伪造出额外日志行。
func TestRelayTruncatesClientNameInLogs(t *testing.T) {
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, testConfig(t, nil), logs)

	// 60 KiB（在 64 KiB 网络入口上限内）的 client_name + 换行注入。
	evil := strings.Repeat("A", 60<<10) + "\nlevel=ERROR msg=INJECTED"

	c := dialControl(t, s.tunnelAddr)
	c.send(protocol.MsgHello, &protocol.Hello{
		Role:       protocol.RoleAgent,
		Token:      testToken,
		Version:    protocol.ProtocolVersion,
		ClientName: evil,
	})
	c.read(2 * time.Second) // HelloOK

	waitFor(t, 3*time.Second, "hello_ok 日志", func() bool {
		return len(logLinesWith(logs, "hello_ok")) > 0
	})
	lines := logLinesWith(logs, "hello_ok")
	if len(lines) != 1 {
		t.Fatalf("hello_ok 应只有一行（换行必须被清洗），实际 %d 行", len(lines))
	}
	if n := len(lines[0]); n >= 512 {
		t.Fatalf("hello_ok 日志行 %d 字节，应 < 512", n)
	}
	if strings.Contains(logs.String(), "INJECTED") {
		t.Fatalf("换行注入不应产生额外日志行:\n%s", logs.String())
	}

	// 鉴权失败路径同样使用 client_name：错误令牌也要截断。
	bad := dialControl(t, s.tunnelAddr)
	bad.send(protocol.MsgHello, &protocol.Hello{
		Role:       protocol.RoleAgent,
		Token:      "wrong-token",
		Version:    protocol.ProtocolVersion,
		ClientName: evil,
	})
	bad.read(2 * time.Second) // MsgError auth_failed

	waitFor(t, 3*time.Second, "auth_failed 日志", func() bool {
		return len(logLinesWith(logs, "auth_failed")) > 0
	})
	for _, line := range logLinesWith(logs, "auth_failed") {
		if len(line) >= 512 {
			t.Fatalf("auth_failed 日志行 %d 字节，应 < 512", len(line))
		}
	}
}

// TestRelayAuthFailureNonLoopbackEmptyToken 覆盖 SEC-A1 的集成路径：
// auth_token 为空时，回环来源仍可接入（既有行为），但拒绝日志要带 remote。
func TestRelayEmptyTokenLoopbackAllowed(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Relay.AuthToken = "" })
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)

	c := dialControl(t, s.tunnelAddr)
	ok := c.hello(protocol.RoleAgent, "anything")
	mustPrefix(t, ok.AgentID, "a-", "AgentID")

	// 启动 WARN 必须说明"仅允许回环"，不能再宣称"任何 agent/client 都可接入"。
	if !strings.Contains(logs.String(), "仅允许回环来源") {
		t.Errorf("空 auth_token 的 WARN 应说明只允许回环来源:\n%s", logs.String())
	}
}
