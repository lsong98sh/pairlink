package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestTCPTunnelTwoSessionsOnOneCarrier 覆盖 udp_mode=tcp_tunnel：
// 一条 TCP 承载连接复用两个 UDP 会话、MsgBindOK 控制帧、单会话 MsgError 不影响其它会话。
func TestTCPTunnelTwoSessionsOnOneCarrier(t *testing.T) {
	echoA := startUDPEchoTag(t, "A:")
	echoB := startUDPEchoTag(t, "B:")
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), []config.ServerService{
		{ID: "default", Proto: "udp", Addr: echoA},
		{ID: "game", Proto: "udp", Addr: echoB},
	}...)
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册 default")
	recv(t, f.registrations, "注册 game")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 2 }, "agent 记录 2 个已注册服务")

	const (
		keyA = "default/sess-a"
		keyB = "game/sess-b"
	)

	// 第一个 UDP 会话 → 承载连接首帧 MsgBind{carrier:udp-over-tcp}。
	if err := f.sendIncoming(keyA, "tok-a", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming A: %v", err)
	}
	first := recv(t, f.tunnelBinds, "承载连接首帧")
	if !first.First {
		t.Fatal("第一条 MsgBind 应标记为首帧")
	}
	if first.Bind.Carrier != protocol.CarrierUDPOverTCP {
		t.Fatalf("首帧 carrier = %q，期望 udp-over-tcp", first.Bind.Carrier)
	}
	if first.Bind.PairingKey != keyA || first.Bind.PairingToken != "tok-a" {
		t.Fatalf("首帧 Bind = %+v", first.Bind)
	}
	if first.Bind.Role != protocol.RoleAgent {
		t.Fatalf("首帧 role = %q", first.Bind.Role)
	}
	carrierConn := recv(t, f.carrierConns, "承载连接")
	waitCond(t, 5*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, keyA))
		return ok && sess.isReady()
	}, "会话 A 进入已建立状态")

	// 第二个 UDP 会话 → 同一承载连接上的隧道帧（session=<配对键> + MsgBind 帧）。
	if err := f.sendIncoming(keyB, "tok-b", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming B: %v", err)
	}
	second := recv(t, f.tunnelBinds, "会话 B 报到帧")
	if second.First {
		t.Fatal("会话 B 的报到帧不应被当作承载连接首帧")
	}
	if second.Session != keyB {
		t.Fatalf("会话 B 的隧道帧 session = %q，期望 %q", second.Session, keyB)
	}
	if second.Bind.PairingKey != keyB || second.Bind.PairingToken != "tok-b" {
		t.Fatalf("会话 B 的 Bind = %+v", second.Bind)
	}
	if second.Bind.Carrier != protocol.CarrierUDPOverTCP {
		t.Fatalf("会话 B 的 Bind.carrier = %q", second.Bind.Carrier)
	}
	waitCond(t, 5*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, keyB))
		return ok && sess.isReady()
	}, "会话 B 进入已建立状态")
	waitCond(t, 3*time.Second, func() bool {
		c := run.a.carrier.current()
		return c != nil && c.sessionCount() == 2
	}, "承载连接上有 2 个会话")

	// 数据面：两个会话都只走这一条承载连接，且各自路由到正确的本地目标。
	if err := f.sendTunnelData(keyA, []byte("data-a")); err != nil {
		t.Fatalf("发送会话 A 数据: %v", err)
	}
	gotA := recv(t, f.tunnelData, "会话 A 回包")
	if gotA.Session != keyA || string(gotA.Payload) != "A:data-a" {
		t.Fatalf("会话 A 回包 = %+v，期望 A:data-a", gotA)
	}
	if err := f.sendTunnelData(keyB, []byte("data-b")); err != nil {
		t.Fatalf("发送会话 B 数据: %v", err)
	}
	gotB := recv(t, f.tunnelData, "会话 B 回包")
	if gotB.Session != keyB || string(gotB.Payload) != "B:data-b" {
		t.Fatalf("会话 B 回包 = %+v，期望 B:data-b", gotB)
	}

	// 单会话错误：session="" + MsgError（detail 携带配对键）只关闭会话 B。
	if err := f.sendTunnelError(protocol.ErrCodeInvalidToken, keyB); err != nil {
		t.Fatalf("发送会话级错误: %v", err)
	}
	waitCond(t, 5*time.Second, func() bool {
		_, ok := run.a.sessions.get(mustKey(t, keyB))
		return !ok
	}, "会话 B 被关闭")
	if c := run.a.carrier.current(); c == nil || c.isClosed() {
		t.Fatal("单会话错误不应关闭整条承载连接")
	}

	// 会话 A 不受影响，继续可以收发。
	if err := f.sendTunnelData(keyA, []byte("data-a2")); err != nil {
		t.Fatalf("发送会话 A 第二个数据: %v", err)
	}
	gotA2 := recv(t, f.tunnelData, "会话 A 第二个回包")
	if gotA2.Session != keyA || string(gotA2.Payload) != "A:data-a2" {
		t.Fatalf("会话 A 第二个回包 = %+v", gotA2)
	}

	// 两个会话只用了 1 条承载连接。
	noValue(t, f.carrierConns, 200*time.Millisecond, "第二条承载连接")

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
	_ = carrierConn.Close()
}

// TestTunnelCarrierLazyRebuild 覆盖承载连接断开后其上会话失效、新会话懒建新连接。
func TestTunnelCarrierLazyRebuild(t *testing.T) {
	echo := startUDPEchoTag(t, "A:")
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "udp", Addr: echo})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	if err := f.sendIncoming("default/first", "tok-1", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	recv(t, f.tunnelBinds, "承载连接首帧")
	carrier1 := recv(t, f.carrierConns, "第一条承载连接")
	waitCond(t, 3*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, "default/first"))
		return ok && sess.isReady()
	}, "首个会话建立")

	// 承载连接断开 → 其上会话全部失效。
	_ = carrier1.Close()
	waitCond(t, 5*time.Second, func() bool { return run.a.sessions.len() == 0 }, "承载断开后会话被清理")

	// 新会话到来时懒建新承载连接（不做会话迁移）。
	if err := f.sendIncoming("default/second", "tok-2", "udp"); err != nil {
		t.Fatalf("下发第二个 MsgIncoming: %v", err)
	}
	second := recv(t, f.tunnelBinds, "新承载连接首帧")
	if !second.First || second.Bind.PairingKey != "default/second" {
		t.Fatalf("新承载连接首帧 = %+v", second)
	}
	recv(t, f.carrierConns, "第二条承载连接")
	waitCond(t, 5*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, "default/second"))
		return ok && sess.isReady()
	}, "新会话建立")

	if err := f.sendTunnelData("default/second", []byte("x")); err != nil {
		t.Fatalf("发送数据: %v", err)
	}
	if got := recv(t, f.tunnelData, "回包"); string(got.Payload) != "A:x" {
		t.Fatalf("回包 = %q，期望 A:x", got.Payload)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestTunnelRejectsUnknownService 覆盖接入通知指向未注册服务时被忽略（不建连接）。
func TestTunnelRejectsUnknownService(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "udp", Addr: startUDPEcho(t)})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	if err := f.sendIncoming("unknown/sess", "tok", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	noValue(t, f.tunnelBinds, 300*time.Millisecond, "承载连接（未注册服务不应建立数据面）")
	if !run.running() {
		t.Fatal("未注册服务的接入通知不应让 Run 退出")
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestIncomingWithoutTokenIgnored 覆盖缺少配对令牌的接入通知被忽略。
func TestIncomingWithoutTokenIgnored(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
		enableUDPData: true,
	})
	run := startAgent(t, newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)}))
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	if err := f.sendIncoming("default/no-token", "", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	noValue(t, f.binds, 300*time.Millisecond, "数据面报到（缺少令牌不应建立）")
	if run.a.sessions.len() != 0 {
		t.Fatalf("缺少令牌的通知不应登记会话，实际 %d", run.a.sessions.len())
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestLogsDoNotContainToken 校验日志中不出现令牌明文（接入密钥与配对令牌都不许打印）。
func TestLogsDoNotContainToken(t *testing.T) {
	const pairToken = "super-secret-pair-token"
	var buf strings.Builder
	log := logx.New(logx.Config{Level: "debug", Format: "text", Output: &buf})

	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	run := startAgentWithLogger(t, cfg, log)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	if err := f.sendIncoming("default/token-leak", pairToken, "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	recv(t, f.binds, "Bind 帧")
	recv(t, f.dataConns, "数据连接")
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("期望至少产生一条日志用于校验")
	}
	if strings.Contains(out, pairToken) {
		t.Fatalf("日志中出现了配对令牌明文")
	}
	if strings.Contains(out, cfg.Server.Token) {
		t.Fatalf("日志中出现了接入密钥明文")
	}
}
