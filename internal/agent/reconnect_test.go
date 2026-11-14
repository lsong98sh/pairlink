package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// logBuffer 是并发安全的日志缓冲（slog 可能从多个 goroutine 写入）。
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

// Write 实现 io.Writer。
func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String 返回日志文本。
func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startAgentWithLogBuffer 启动 agent 并把日志写入缓冲，便于断言事件。
func startAgentWithLogBuffer(t *testing.T, cfg *config.Config) (*agentRun, *logBuffer) {
	t.Helper()
	buf := &logBuffer{}
	log := logx.New(logx.Config{Level: "debug", Format: "text", Output: buf})
	return startAgentWithLogger(t, cfg, log), buf
}

// requireLogEvent 断言日志里出现 event=<name>。
func requireLogEvent(t *testing.T, buf *logBuffer, name string) {
	t.Helper()
	waitCond(t, 3*time.Second, func() bool {
		return strings.Contains(buf.String(), "event="+name)
	}, "日志事件 "+name)
}

// TestReconnectAfterControlDrop 覆盖 A1 核心：控制连接断开 → 自动重连 →
// 重新注册全部服务 → 服务恢复可用（TCP echo 走通）。
func TestReconnectAfterControlDrop(t *testing.T) {
	upstream := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: upstream})
	cfg.Server.Reconnect = true
	run, logs := startAgentWithLogBuffer(t, cfg)

	recv(t, f.registrations, "首次注册请求")
	waitCond(t, 3*time.Second, func() bool { return run.a.controlSnapshot().connected }, "控制连接已建立")

	// 断开控制连接（非踢出、非 relay 停机）。
	f.closeControl()
	waitCond(t, 5*time.Second, func() bool { return !run.a.controlSnapshot().connected }, "控制连接已断开")

	// 重连后必须重新注册：fake relay 会收到第二条注册请求。
	recv(t, f.registrations, "重连后的注册请求")
	waitCond(t, 5*time.Second, func() bool {
		snap := run.a.controlSnapshot()
		return snap.connected && len(run.a.registeredServices()) == 1
	}, "重连完成并重新注册服务")
	if got := f.tunnelConns(); got != 2 {
		t.Fatalf("隧道连接数 = %d，期望 2（首次 + 重连）", got)
	}
	requireLogEvent(t, logs, "reconnect_scheduled")
	requireLogEvent(t, logs, "reconnect_ok")

	// 服务恢复可用：下发接入通知 → TCP 数据面 echo 走通。
	if err := f.sendIncoming("default/reconnect-1", "reconnect-token", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	dataConn := recv(t, f.dataConns, "重连后的 TCP 数据面连接")
	recv(t, f.binds, "TCP 数据面报到帧")
	payload := []byte("after-reconnect\n")
	writeAll(t, dataConn, payload, "relay→agent 数据")
	if got := readN(t, dataConn, len(payload), "echo 回包"); string(got) != string(payload) {
		t.Fatalf("echo 回包 = %q，期望 %q", got, payload)
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("ctx 取消后 Run 应返回 nil，实际: %v", err)
	}
}

// TestReconnectDisabledReturnsError 覆盖 reconnect=false 的旧行为：
// 控制连接断开 → Run 返回错误（不重连）。
func TestReconnectDisabledReturnsError(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	cfg.Server.Reconnect = false
	run, logs := startAgentWithLogBuffer(t, cfg)

	recv(t, f.registrations, "注册请求")
	f.closeControl()

	err := run.wait(t)
	if err == nil {
		t.Fatal("reconnect=false 时控制连接断开应返回错误")
	}
	if !strings.Contains(err.Error(), "控制连接已断开") {
		t.Fatalf("错误应说明控制连接断开，实际: %v", err)
	}
	if strings.Contains(logs.String(), "reconnect_scheduled") {
		t.Fatal("reconnect=false 时不应安排重连")
	}
	// 不应出现第二条隧道连接。
	if got := f.tunnelConns(); got != 1 {
		t.Fatalf("隧道连接数 = %d，期望 1", got)
	}
}

// TestReconnectStopsOnKicked 覆盖踢出不重连：MsgKicked → 清理后 Run 返回错误。
func TestReconnectStopsOnKicked(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	cfg.Server.Reconnect = true
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
	// 等一小会儿，确认没有重连（没有第二条隧道连接、没有新的注册请求）。
	time.Sleep(200 * time.Millisecond)
	if got := f.tunnelConns(); got != 1 {
		t.Fatalf("踢出后不应重连：隧道连接数 = %d", got)
	}
	noValue(t, f.registrations, 100*time.Millisecond, "踢出后的注册请求")
	if run.a.sessions.len() != 0 {
		t.Fatalf("踢出后应清理全部会话，实际残留 %d 个", run.a.sessions.len())
	}
}

// TestReconnectSurvivesUnreachableRelay 覆盖 relay 不可达时进程存活且退避等待可被 ctx 取消。
func TestReconnectSurvivesUnreachableRelay(t *testing.T) {
	cfg := newTestConfig(closedTCPAddr(t),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	cfg.Server.Reconnect = true
	run, logs := startAgentWithLogBuffer(t, cfg)

	requireLogEvent(t, logs, "reconnect_failed")
	if !run.running() {
		t.Fatal("relay 不可达且 reconnect=true 时 Run 不应退出")
	}
	requireLogEvent(t, logs, "reconnect_scheduled")

	// ctx 取消必须能打断退避等待并返回 nil。
	if err := run.stop(t); err != nil {
		t.Fatalf("ctx 取消后 Run 应返回 nil，实际: %v", err)
	}
}

// TestNativeUDPBindOKLostRetry 覆盖原生 UDP 报到丢包：
// 前 1 个 BindOK 被丢弃后 agent 每 ~300ms 重发报到帧并最终建立数据面。
func TestNativeUDPBindOKLostRetry(t *testing.T) {
	upstream := startUDPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
		enableUDPData: true,
		dropBindOK:    1,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "udp", Addr: upstream})
	run := startAgent(t, cfg)
	recv(t, f.registrations, "注册请求")

	const key = "default/udp-lost-bindok"
	if err := f.sendIncoming(key, "udp-token", "udp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	// 第一次报到（BindOK 被丢弃）。
	first := recv(t, f.udpBinds, "第一次 UDP 报到")
	if first.Bind.PairingKey != key {
		t.Fatalf("报到配对键 = %q", first.Bind.PairingKey)
	}
	// agent 应重发（同一配对键），第二次才收到 BindOK。
	start := time.Now()
	second := recv(t, f.udpBinds, "重发的 UDP 报到")
	if second.Bind.PairingKey != key {
		t.Fatalf("重发配对键 = %q", second.Bind.PairingKey)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("重发间隔过长：%s（期望约 300ms）", elapsed)
	}

	waitCond(t, 3*time.Second, func() bool {
		sess, ok := run.a.sessions.get(mustKey(t, key))
		return ok && sess.isReady()
	}, "会话在重发后建立")

	// 数据面可用：relay → agent → 本地 UDP echo → 回 relay。
	if err := f.sendUDP([]byte("ping")); err != nil {
		t.Fatalf("relay 发送数据报: %v", err)
	}
	select {
	case data := <-f.udpData:
		if string(data) != "ping" {
			t.Fatalf("回包 = %q，期望 ping", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 UDP 回包超时")
	}

	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// TestHeartbeatRebuiltAfterReconnect 校验重连后心跳在新连接上重建。
func TestHeartbeatRebuiltAfterReconnect(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
		heartbeatSecs: 1,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	cfg.Server.Heartbeat = time.Hour // relay 建议 1s，实际取 min → 1s
	cfg.Server.Reconnect = true
	run := startAgent(t, cfg)

	recv(t, f.registrations, "首次注册请求")
	f.closeControl()
	recv(t, f.registrations, "重连后的注册请求")
	waitCond(t, 5*time.Second, func() bool { return run.a.controlSnapshot().connected }, "重连完成")

	// 新连接上的心跳：fake relay 记录心跳时间戳。
	before := f.heartbeatCount()
	waitCond(t, 4*time.Second, func() bool { return f.heartbeatCount() > before }, "重连后仍有心跳")
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}

// heartbeatCount 返回假 relay 收到的心跳帧总数。
func (f *fakeRelay) heartbeatCount() int { return int(f.heartbeats.Load()) }
