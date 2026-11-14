package relay

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// lockedBuffer 是并发安全的日志缓冲（slog 从多个 goroutine 写入）。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write 实现 io.Writer。
func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String 返回当前日志文本。
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startRelayWithLogs 启动 relay 并把日志替换为可观测的 debug logger。
//
// newServer 会自建 logger，因此必须在 serve 之前替换 s.log（只影响该测试实例）。
func startRelayWithLogs(t *testing.T, cfg *config.Config, logs *lockedBuffer) *server {
	t.Helper()
	s := newServer(cfg)
	s.log = logx.New(logx.Config{Level: "debug", Format: "text", Mode: "relay", Output: logs})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.serve(ctx) }()
	select {
	case <-s.started:
	case err := <-errCh:
		cancel()
		t.Fatalf("relay 启动失败: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("relay 启动超时")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("relay 退出超时")
		}
	})
	return s
}

// TestRelayLogsDoNotContainToken 校验 relay 在**debug 级别**下也不打印
// 一次性配对令牌与隧道接入密钥的明文（docs/contract.md §4.7）。
func TestRelayLogsDoNotContainToken(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, nil)
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-log", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "svc-log", "conn-log")

	pairToken := result.PairingToken
	if pairToken == "" || incoming.PairingToken != pairToken {
		t.Fatalf("令牌异常: result=%q incoming=%q", pairToken, incoming.PairingToken)
	}

	// 1) 错误令牌：触发 bind_fail 日志（错误链里绝不能带明文令牌）。
	badToken := "wrong-" + pairToken
	bad := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: badToken,
		Role:         protocol.RoleClient,
	})
	expectClosed(t, bad, 2*time.Second)

	// 2) 正常配对 + 转发：触发 pair_issue / pair_complete / data_close。
	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	echoRoundTrip(t, clientData, agentData, "log-leak-check")
	_ = clientData.Close()
	_ = agentData.Close()
	waitFor(t, 3*time.Second, "配对表清理", func() bool { return s.pairs.Len() == 0 })

	out := logs.String()
	if out == "" {
		t.Fatal("期望至少产生一条日志用于校验")
	}
	for _, want := range []string{"pair_issue", "pair_complete", "bind_fail"} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺少关键事件 %q", want)
		}
	}
	if strings.Contains(out, pairToken) {
		t.Fatal("日志中出现了配对令牌明文")
	}
	if strings.Contains(out, cfg.Relay.AuthToken) {
		t.Fatal("日志中出现了隧道接入密钥（auth_token）明文")
	}
}

// TestRelayIdleTimeoutLogsEvent 证明 idle 超时会在 relay 日志里留下可检索的事件
// （event=idle_timeout），而不是混在普通 data_close 里说不清原因。
func TestRelayIdleTimeoutLogsEvent(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.IdleTimeout = 200 * time.Millisecond
		c.Relay.PairTimeout = 2 * time.Second
	})
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-idle", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "svc-idle", "conn-idle")

	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	t.Cleanup(func() {
		_ = clientData.Close()
		_ = agentData.Close()
	})

	// 不产生任何流量：idle_timeout=200ms 后转发必须结束并记录 idle_timeout。
	waitFor(t, 5*time.Second, "空闲超时后配对表清理", func() bool { return s.pairs.Len() == 0 })
	waitFor(t, 3*time.Second, "日志出现 idle_timeout 事件", func() bool {
		return strings.Contains(logs.String(), "idle_timeout")
	})
	if !strings.Contains(logs.String(), "数据面空闲超时") {
		t.Fatalf("idle 超时日志缺少可读说明；日志尾部:\n%s", tail(logs.String(), 2000))
	}
}

// tail 返回字符串末尾最多 n 字节（便于失败时看日志）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
