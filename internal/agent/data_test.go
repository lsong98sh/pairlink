package agent

import (
	"bytes"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestTCPIncomingBindAndForward 覆盖 MsgIncoming → TCP 数据面拨号 + Bind 帧内容
// + 与假 echo 上游的双向转发。
func TestTCPIncomingBindAndForward(t *testing.T) {
	upstream := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: upstream})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	const (
		key   = "default/abcd1234"
		token = "pair-token-abc"
	)
	if err := f.sendIncoming(key, token, "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}

	bind := recv(t, f.binds, "TCP 数据面报到帧")
	if bind.PairingKey != key {
		t.Fatalf("Bind.pairing_key = %q，期望 %q", bind.PairingKey, key)
	}
	if bind.PairingToken != token {
		t.Fatalf("Bind.pairing_token = %q，期望 %q", bind.PairingToken, token)
	}
	if bind.Role != protocol.RoleAgent {
		t.Fatalf("Bind.role = %q，期望 agent", bind.Role)
	}
	if bind.Carrier != protocol.CarrierTCP && bind.Carrier != "" {
		t.Fatalf("Bind.carrier = %q，期望 tcp（或空）", bind.Carrier)
	}

	dataConn := recv(t, f.dataConns, "TCP 数据面连接")
	waitCond(t, 3*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, key))
		return ok && sess.isReady()
	}, "会话进入已建立状态")

	payload := []byte("hello-pairlink\n")
	writeAll(t, dataConn, payload, "relay→agent 数据")
	if got := readN(t, dataConn, len(payload), "echo 回包（agent→relay）"); !bytes.Equal(got, payload) {
		t.Fatalf("echo 回包 = %q，期望 %q", got, payload)
	}

	// 第二个方向验证：再发一次，确保连接持续可用（不是一次性）。
	payload2 := []byte("second-round\n")
	writeAll(t, dataConn, payload2, "relay→agent 数据（第二次）")
	if got := readN(t, dataConn, len(payload2), "echo 回包（第二次）"); !bytes.Equal(got, payload2) {
		t.Fatalf("echo 回包 = %q，期望 %q", got, payload2)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
	if run.a.sessions.len() != 0 {
		t.Fatalf("关停后会话表应为空，实际 %d", run.a.sessions.len())
	}
}

// TestUpstreamDialFailureOnlyAffectsPairing 覆盖本地目标拨号失败只影响该配对，Run 不退出。
func TestUpstreamDialFailureOnlyAffectsPairing(t *testing.T) {
	goodUpstream := startTCPEcho(t)
	badUpstream := closedTCPAddr(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), []config.ServerService{
		{ID: "default", Proto: "tcp", Addr: badUpstream},
		{ID: "good", Proto: "tcp", Addr: goodUpstream},
	}...)
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册 default")
	recv(t, f.registrations, "注册 good")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 2 }, "agent 记录 2 个已注册服务")

	// 第一个配对：本地目标不可用。
	if err := f.sendIncoming("default/broken", "tok-broken", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	recv(t, f.binds, "坏配对的 Bind 帧")
	badConn := recv(t, f.dataConns, "坏配对的数据连接")
	// agent 拨本地目标失败后应关闭数据连接。
	expectEOF(t, badConn, "上游拨号失败后关闭数据连接")
	if !run.running() {
		t.Fatalf("上游拨号失败不应让 Run 退出")
	}
	waitCond(t, 3*time.Second, func() bool { return run.a.sessions.len() == 0 }, "失败会话被清理")

	// 第二个配对：仍然正常工作。
	if err := f.sendIncoming("good/ok1", "tok-ok", "tcp"); err != nil {
		t.Fatalf("下发第二个 MsgIncoming: %v", err)
	}
	bind := recv(t, f.binds, "好配对的 Bind 帧")
	if bind.PairingKey != "good/ok1" {
		t.Fatalf("Bind.pairing_key = %q", bind.PairingKey)
	}
	goodConn := recv(t, f.dataConns, "好配对的数据连接")
	payload := []byte("after-failure\n")
	writeAll(t, goodConn, payload, "relay→agent 数据")
	if got := readN(t, goodConn, len(payload), "echo 回包"); !bytes.Equal(got, payload) {
		t.Fatalf("echo 回包 = %q，期望 %q", got, payload)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestNativeUDPForwardAndIdleCleanup 覆盖原生 UDP：报到、BindOK、双向数据报、空闲清理。
func TestNativeUDPForwardAndIdleCleanup(t *testing.T) {
	udpEcho := startUDPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
		enableUDPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "udp", Addr: udpEcho})
	cfg.Server.UDPSessionTimeout = 250 * time.Millisecond
	before := runtime.NumGoroutine()
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	const (
		key   = "default/udp1"
		token = "tok-udp"
	)
	if err := f.sendIncoming(key, token, "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	bind := recv(t, f.udpBinds, "UDP 报到数据报")
	if bind.Bind.PairingKey != key || bind.Bind.PairingToken != token {
		t.Fatalf("UDP Bind = %+v", bind.Bind)
	}
	if bind.Bind.Role != protocol.RoleAgent {
		t.Fatalf("UDP Bind.role = %q", bind.Bind.Role)
	}
	if bind.Bind.Carrier != "" {
		t.Fatalf("原生 UDP Bind.carrier 应为空，实际 %q", bind.Bind.Carrier)
	}
	waitCond(t, 3*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, key))
		return ok && sess.isReady()
	}, "UDP 会话进入已建立状态")

	// relay → agent → 本地 echo → agent → relay
	if err := f.sendUDP([]byte("ping-udp")); err != nil {
		t.Fatalf("发送 UDP 数据报: %v", err)
	}
	got := recv(t, f.udpData, "UDP echo 回包")
	if string(got) != "ping-udp" {
		t.Fatalf("UDP 回包 = %q，期望 %q", got, "ping-udp")
	}

	// 空闲清理：250ms 无流量后会话应被关闭并移除。
	waitCond(t, 5*time.Second, func() bool { return run.a.sessions.len() == 0 }, "UDP 会话空闲清理")

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
	checkNoGoroutineLeak(t, before)
}

// TestNativeUDPBindTimeout 覆盖 pair_timeout 内没收到 MsgBindOK 时放弃本次配对。
func TestNativeUDPBindTimeout(t *testing.T) {
	udpEcho := startUDPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
		enableUDPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "udp", Addr: udpEcho})
	cfg.Relay.PairTimeout = 150 * time.Millisecond
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	// 先把假 relay 的 UDP 读循环关闭，使 BindOK 永远不会到达。
	_ = f.udpConn.Close()
	if err := f.sendIncoming("default/udp-timeout", "tok", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	// agent 端会因超时放弃该配对并清理会话。
	waitCond(t, 5*time.Second, func() bool { return run.a.sessions.len() == 0 }, "BindOK 超时后清理会话")
	if !run.running() {
		t.Fatal("BindOK 超时不应让 Run 退出")
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestShutdownNoGoroutineLeak 覆盖关停后 goroutine 不增长。
func TestShutdownNoGoroutineLeak(t *testing.T) {
	upstream := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: upstream})

	before := runtime.NumGoroutine()
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")

	if err := f.sendIncoming("default/leak1", "tok", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	recv(t, f.binds, "Bind 帧")
	dataConn := recv(t, f.dataConns, "数据连接")
	payload := []byte("x\n")
	writeAll(t, dataConn, payload, "数据")
	readN(t, dataConn, len(payload), "回包")

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
	checkNoGoroutineLeak(t, before)
}

// TestAdminPage 覆盖只读管理页展示服务、控制连接与活跃数据连接。
func TestAdminPage(t *testing.T) {
	upstream := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		agentID:       "a-fedcba9876543210",
		enableTCPData: true,
		enableUDPData: true,
	})
	manageAddr := freeTCPAddr(t)
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: upstream})
	cfg.Common.ManageListen = manageAddr
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 记录已注册服务")

	const key = "default/admin1"
	if err := f.sendIncoming(key, "tok", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	recv(t, f.binds, "Bind 帧")
	dataConn := recv(t, f.dataConns, "数据连接")
	defer dataConn.Close()
	waitCond(t, 3*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, key))
		return ok && sess.isReady()
	}, "会话进入已建立状态")

	// 管理页可能比 agent 稍晚就绪，轮询直到 200。
	url := "http://" + manageAddr + "/status"
	var body string
	waitCond(t, 5*time.Second, func() bool {
		got, ok := tryHTTPGet(url)
		if ok {
			body = got
		}
		return ok && got != ""
	}, "管理页可访问")

	for _, want := range []string{
		"本机已注册服务", "default", "tcp", upstream,
		"控制连接", "a-fedcba9876543210", "udp_mode", "native",
		"当前活跃数据连接", key,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("管理页缺少 %q；实际内容片段：%s", want, truncate(body, 400))
		}
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestConcurrentPairingsIndependent 覆盖同一 agent 上并发多组配对互不干扰。
func TestConcurrentPairingsIndependent(t *testing.T) {
	echo1 := startTCPEcho(t)
	echo2 := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), []config.ServerService{
		{ID: "default", Proto: "tcp", Addr: echo1},
		{ID: "web", Proto: "tcp", Addr: echo2},
	}...)
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册 default")
	recv(t, f.registrations, "注册 web")
	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 2 }, "agent 记录 2 个服务")

	const (
		key1 = "default/c1"
		key2 = "web/c2"
	)
	if err := f.sendIncoming(key1, "tok-1", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming 1: %v", err)
	}
	if err := f.sendIncoming(key2, "tok-2", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming 2: %v", err)
	}

	conns := map[string]net.Conn{}
	for i := 0; i < 2; i++ {
		bind := recv(t, f.binds, "Bind 帧")
		conns[bind.PairingKey] = recv(t, f.dataConns, "数据连接")
	}
	if len(conns) != 2 {
		t.Fatalf("期望 2 个不同配对键的数据连接，实际: %v", len(conns))
	}
	waitCond(t, 3*time.Second, func() bool { return run.a.sessions.len() == 2 }, "两个会话都已登记")

	// 两组连接各自独立收发，互不影响。
	for key, payload := range map[string]string{
		key1: "payload-one\n",
		key2: "payload-two\n",
	} {
		writeAll(t, conns[key], []byte(payload), "写入 "+key)
		if got := readN(t, conns[key], len(payload), "回包 "+key); string(got) != payload {
			t.Fatalf("%s 回包 = %q，期望 %q", key, got, payload)
		}
	}

	// 重复的 MsgIncoming（同一配对键）应被忽略，不会新建数据连接。
	if err := f.sendIncoming(key1, "tok-1", "tcp"); err != nil {
		t.Fatalf("下发重复 MsgIncoming: %v", err)
	}
	noValue(t, f.binds, 300*time.Millisecond, "重复配对的数据面报到帧")
	if run.a.sessions.len() != 2 {
		t.Fatalf("重复通知不应改变会话数，实际 %d", run.a.sessions.len())
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// mustKey 把配对键字符串转为 PairingKey（测试辅助）。
func mustKey(t *testing.T, s string) pairing.PairingKey {
	t.Helper()
	key, err := pairing.ParsePairingKey(s)
	if err != nil {
		t.Fatalf("解析配对键 %q: %v", s, err)
	}
	return key
}

// truncate 截断字符串用于失败信息。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
