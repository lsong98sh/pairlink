package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestHelloAndRegisterAllServices 覆盖 hello 成功 + 多服务注册（default + 2 个附加服务）。
func TestHelloAndRegisterAllServices(t *testing.T) {
	tcpEcho1 := startTCPEcho(t)
	tcpEcho2 := startTCPEcho(t)
	udpEcho := startUDPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		agentID:       "a-0123456789abcdef",
		enableTCPData: true,
		enableUDPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), config.ServerService{ID: "default", Proto: "tcp", Addr: tcpEcho1})
	cfg.Server.Services = []config.ServerService{
		{ID: "web", Proto: "tcp", Addr: tcpEcho2},
		{ID: "game", Proto: "udp", Addr: udpEcho},
	}
	run := startAgent(t, cfg)

	// 注册顺序必须是 default（主服务）在前。
	wantIDs := []string{"default", "web", "game"}
	for _, id := range wantIDs {
		req := recv(t, f.registrations, "注册请求 "+id)
		if req.ServiceID != id {
			t.Fatalf("注册顺序错误：期望 %q，收到 %q", id, req.ServiceID)
		}
		if req.Proto != "" && req.LocalHint == "" {
			t.Fatalf("服务 %q 的 local_hint 为空", id)
		}
	}
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 3 }, "agent 记录 3 个已注册服务")

	if got := run.a.udpModeSnapshot(); got != protocol.UDPModeNative {
		t.Fatalf("udp_mode = %q，期望 native", got)
	}
	snap := run.a.controlSnapshot()
	if snap.agentID != "a-0123456789abcdef" {
		t.Fatalf("agent_id = %q", snap.agentID)
	}
	if !snap.connected {
		t.Fatal("控制连接状态应为已连接")
	}
	if snap.tcpDataPort != f.tcpPort() || snap.udpDataPort != f.udpPort() {
		t.Fatalf("数据面端口未记录：tcp=%d udp=%d，期望 %d/%d",
			snap.tcpDataPort, snap.udpDataPort, f.tcpPort(), f.udpPort())
	}
	// HelloOK.heartbeat_seconds = 0（未建议）→ 使用本地配置 1h。
	if snap.heartbeat != time.Hour {
		t.Fatalf("心跳间隔 = %s，期望 1h", snap.heartbeat)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("ctx 取消后 Run 应返回 nil，实际: %v", err)
	}
}

// TestHelloVersionMismatch 覆盖 HelloOK 版本不匹配。
func TestHelloVersionMismatch(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		helloVersion:  protocol.ProtocolVersion + 1,
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)

	err := run.wait(t)
	if err == nil {
		t.Fatal("版本不匹配时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Fatalf("错误信息应包含「版本」，实际: %v", err)
	}
}

// TestHelloTokenRejected 覆盖令牌被 relay 拒绝。
func TestHelloTokenRejected(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "another-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)

	err := run.wait(t)
	if err == nil {
		t.Fatal("令牌错误时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "拒绝接入") || !strings.Contains(err.Error(), protocol.ErrCodeAuthFailed) {
		t.Fatalf("错误信息应包含拒绝接入与 auth_failed，实际: %v", err)
	}
}

// TestRegisterServiceError 覆盖注册被 relay 拒绝时 Run 返回错误。
func TestRegisterServiceError(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		failServiceID: "web",
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	cfg.Server.Services = []config.ServerService{
		{ID: "web", Proto: "tcp", Addr: startTCPEcho(t)},
	}
	run := startAgent(t, cfg)

	err := run.wait(t)
	if err == nil {
		t.Fatal("注册被拒绝时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "注册服务") {
		t.Fatalf("错误信息应说明注册失败，实际: %v", err)
	}
}

// TestUnknownUDPModeFallsBackToNative 覆盖未知 udp_mode 回退 native。
func TestUnknownUDPModeFallsBackToNative(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       "future-mode",
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)

	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")
	if got := run.a.udpModeSnapshot(); got != protocol.UDPModeNative {
		t.Fatalf("未知 udp_mode 应回退 native，实际: %q", got)
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestMissingUDPModeWarnsAndFallsBack 覆盖 relay 未下发 udp_mode 时的回退。
func TestMissingUDPModeWarnsAndFallsBack(t *testing.T) {
	f := newFakeRelay(t, relayOptions{token: "test-token", enableTCPData: true})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)

	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")
	if got := run.a.udpModeSnapshot(); got != protocol.UDPModeNative {
		t.Fatalf("缺失 udp_mode 应回退 native，实际: %q", got)
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestKicked 覆盖 MsgKicked：清理后 Run 返回错误且不重连。
func TestKicked(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")

	if err := f.sendKicked("管理页踢出"); err != nil {
		t.Fatalf("下发 MsgKicked: %v", err)
	}
	err := run.wait(t)
	if err == nil {
		t.Fatal("被踢出时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "踢出") {
		t.Fatalf("错误应说明被踢出，实际: %v", err)
	}
	// 不重连：不重新拨号 = 不会出现第二条隧道连接。
	if run.a.sessions.len() != 0 {
		t.Fatalf("踢出后应清理全部会话，实际残留 %d 个", run.a.sessions.len())
	}
}

// TestServerShutdown 覆盖 MsgServerShutdown：清理后 Run 返回 nil。
func TestServerShutdown(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")

	if err := f.sendServerShutdown("relay 停机维护"); err != nil {
		t.Fatalf("下发 MsgServerShutdown: %v", err)
	}
	if err := run.wait(t); err != nil {
		t.Fatalf("relay 停机时 Run 应返回 nil，实际: %v", err)
	}
}

// TestControlConnectionLost 覆盖控制连接意外断开 + reconnect=false（旧行为）：
// 返回错误、不自动重连。reconnect=true 的自愈语义见 reconnect_test.go。
func TestControlConnectionLost(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	// 直接构造 Config 时 Reconnect 零值为 false（= 阶段 6C 之前的旧行为）。
	cfg.Server.Reconnect = false
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	// 等首次注册确认被 agent 消化（ctrlReady）：否则断链可能落在"等确认"阶段，
	// 错误措辞会变成注册超时而非控制连接断开（-race 下会随机复现）。
	waitCond(t, 5*time.Second, func() bool { return run.a.ctrlReady.Load() }, "控制连接就绪")

	f.closeControl()
	err := run.wait(t)
	if err == nil {
		t.Fatal("控制连接断开时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "控制连接已断开") {
		t.Fatalf("错误应说明控制连接断开，实际: %v", err)
	}
}

// TestDataAddrDerivation 覆盖 §4.5 的地址推导（IPv4 / IPv6 / 域名）。
func TestDataAddrDerivation(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
		bad  bool
	}{
		{host: "127.0.0.1", port: 7001, want: "127.0.0.1:7001"},
		{host: "::1", port: 7001, want: "[::1]:7001"},
		{host: "relay.example.com", port: 7001, want: "relay.example.com:7001"},
		{host: "", port: 7001, bad: true},
		{host: "relay.example.com", port: 0, bad: true},
		{host: "relay.example.com", port: 70000, bad: true},
	}
	for _, c := range cases {
		got, err := dataAddr(c.host, c.port)
		if c.bad {
			if err == nil {
				t.Fatalf("dataAddr(%q,%d) 应返回错误，实际: %q", c.host, c.port, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("dataAddr(%q,%d): %v", c.host, c.port, err)
		}
		if got != c.want {
			t.Fatalf("dataAddr(%q,%d) = %q，期望 %q", c.host, c.port, got, c.want)
		}
	}
}

// TestListenPortFallback 覆盖从 [relay] 段回退取端口的解析。
func TestListenPortFallback(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{addr: "0.0.0.0:7001", want: 7001},
		{addr: "127.0.0.1:7002", want: 7002},
		{addr: "", want: 0},
		{addr: "not-an-addr", want: 0},
		{addr: "127.0.0.1:0", want: 0},
	}
	for _, c := range cases {
		if got := listenPort(c.addr); got != c.want {
			t.Fatalf("listenPort(%q) = %d，期望 %d", c.addr, got, c.want)
		}
	}
}
