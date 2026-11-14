package client

import (
	"net"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// newUDPClient 建一个连到本地监听地址的「第三方」UDP socket。
func newUDPClient(t *testing.T, listenAddr string) *net.UDPConn {
	t.Helper()
	raddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		t.Fatalf("解析本地监听地址失败: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		t.Fatalf("连接本地 UDP 监听失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readUDPWithTimeout 从第三方 UDP socket 读一个数据报。
func readUDPWithTimeout(t *testing.T, conn *net.UDPConn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读取 UDP 回包失败: %v", err)
	}
	return buf[:n]
}

// decodeBindFrame 解析 MsgBind 帧。
func decodeBindFrame(t *testing.T, data []byte) *protocol.Bind {
	t.Helper()
	typ, payload, err := protocol.ParseFrame(data)
	if err != nil {
		t.Fatalf("解析数据报帧失败: %v", err)
	}
	if typ != protocol.MsgBind {
		t.Fatalf("期望 MsgBind，收到 %s", typ)
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		t.Fatalf("解码 MsgBind 失败: %v", err)
	}
	bind, ok := msg.(*protocol.Bind)
	if !ok {
		t.Fatalf("MsgBind 解码类型异常")
	}
	return bind
}

// readTunnelFrame 从承载连接读一个 UDP 隧道帧。
func readTunnelFrame(t *testing.T, conn net.Conn, timeout time.Duration) (string, []byte) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	session, payload, err := protocol.ReadUDPTunnelFrame(conn)
	if err != nil {
		t.Fatalf("读取 UDP 隧道帧失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return session, payload
}

// writeTunnelFrame 向承载连接写一个 UDP 隧道帧。
func writeTunnelFrame(t *testing.T, conn net.Conn, session string, payload []byte) {
	t.Helper()
	if err := protocol.WriteUDPTunnelFrame(conn, session, payload); err != nil {
		t.Fatalf("写 UDP 隧道帧失败: %v", err)
	}
}

// TestNativeUDP 覆盖原生 UDP：本地会话 → 报到 → 双向数据报 → 空闲清理 → 重新建立。
func TestNativeUDP(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{udpService("svc-udp")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
		withUDP:  true,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 1 * time.Second
	cfg.Client.UDPSessionTimeout = 300 * time.Millisecond
	cfg.Client.Services = []config.ClientService{{ID: "svc-udp", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	listenAddr := tc.listenerAddr("svc-udp")
	third := newUDPClient(t, listenAddr)

	// 1) 首个数据报触发会话建立与 ConnectRequest。
	if _, err := third.Write([]byte("hello-udp")); err != nil {
		t.Fatalf("第三方发送数据报失败: %v", err)
	}
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key := "svc-udp/" + cr.ClientConnID
	if cr.ServiceID != "svc-udp" {
		t.Fatalf("ConnectRequest.ServiceID = %q", cr.ServiceID)
	}

	// 2) relay 下发配对结果。
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key,
		PairingToken: "udp-token",
		Proto:        "udp",
		Role:         protocol.RoleClient,
	})

	// 3) client 发报到数据报。
	dg := r.nextDatagram(t, 5*time.Second)
	bind := decodeBindFrame(t, dg.data)
	if bind.PairingKey != key || bind.PairingToken != "udp-token" {
		t.Fatalf("UDP Bind 内容异常: %+v", bind)
	}
	if bind.Role != protocol.RoleClient {
		t.Fatalf("UDP Bind.Role = %q", bind.Role)
	}
	if bind.Carrier != "" {
		t.Fatalf("原生 UDP 的 Bind.Carrier 应为空，实际 %q", bind.Carrier)
	}

	// 4) relay 回 BindOK。
	okFrame, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key})
	if err != nil {
		t.Fatalf("编码 BindOK 失败: %v", err)
	}
	r.udpSend(t, dg.from, okFrame)

	// 5) 配对期间缓存的首包被补发。
	dg2 := r.nextDatagram(t, 5*time.Second)
	if string(dg2.data) != "hello-udp" {
		t.Fatalf("首包补发内容 = %q, 期望 hello-udp", dg2.data)
	}

	// 6) relay → client → 第三方。
	r.udpSend(t, dg2.from, []byte("world-udp"))
	if got := readUDPWithTimeout(t, third, 3*time.Second); string(got) != "world-udp" {
		t.Fatalf("第三方收到 %q, 期望 world-udp", got)
	}

	// 7) 第三方 → client → relay。
	if _, err := third.Write([]byte("again")); err != nil {
		t.Fatalf("第三方发送数据报失败: %v", err)
	}
	dg3 := r.nextDatagram(t, 5*time.Second)
	if string(dg3.data) != "again" {
		t.Fatalf("转发到 relay 的数据 = %q, 期望 again", dg3.data)
	}

	// 8) 空闲清理。
	waitFor(t, 5*time.Second, "UDP 会话空闲清理", func() bool { return tc.c.udpSessionCount() == 0 })
	tc.logContains(t, "idle_timeout")
	tc.logContains(t, "event=bind")
	tc.logContains(t, "udp_session_open")
	tc.logContains(t, "udp_session_close")

	// 9) 旧会话清理后，同一源地址再次发包会建立新会话（新 client_conn_id）。
	if _, err := third.Write([]byte("after-idle")); err != nil {
		t.Fatalf("第三方再次发送数据报失败: %v", err)
	}
	cr2 := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if cr2.ClientConnID == cr.ClientConnID {
		t.Fatalf("空闲清理后应使用新的 client_conn_id")
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestUDPTunnel 覆盖 UDP over TCP：一条承载连接复用两个会话、控制帧、
// 单会话错误隔离、承载断开后会话失效与懒建。
func TestUDPTunnel(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{udpService("svc-u2")},
		revision: 1,
		udpMode:  protocol.UDPModeTCPTunnel,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 2 * time.Second
	cfg.Client.Services = []config.ClientService{{ID: "svc-u2", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	listenAddr := tc.listenerAddr("svc-u2")
	thirdA := newUDPClient(t, listenAddr)
	thirdB := newUDPClient(t, listenAddr)

	// ---- 会话 1：首帧 MsgBind{carrier:udp-over-tcp} 建立承载连接 ----
	if _, err := thirdA.Write([]byte("A1")); err != nil {
		t.Fatalf("第三方 A 发送失败: %v", err)
	}
	cr1 := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key1 := "svc-u2/" + cr1.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key1,
		PairingToken: "token-1",
		Proto:        "udp",
		Role:         protocol.RoleClient,
	})

	dc := r.nextDataConn(t, 5*time.Second)
	if dc.bind.Carrier != protocol.CarrierUDPOverTCP {
		t.Fatalf("承载首帧 Carrier = %q, 期望 udp-over-tcp", dc.bind.Carrier)
	}
	if dc.bind.PairingKey != key1 {
		t.Fatalf("承载首帧 PairingKey = %q, 期望 %q", dc.bind.PairingKey, key1)
	}
	if dc.bind.Role != protocol.RoleClient {
		t.Fatalf("承载首帧 Role = %q", dc.bind.Role)
	}

	// relay 回控制帧 BindOK（session=""）。
	bindOK, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key1})
	if err != nil {
		t.Fatalf("编码 BindOK 失败: %v", err)
	}
	writeTunnelFrame(t, dc.conn, "", bindOK)

	// 会话 1 的首包作为数据帧发出。
	session, payload := readTunnelFrame(t, dc.conn, 5*time.Second)
	if session != key1 || string(payload) != "A1" {
		t.Fatalf("会话 1 首帧 = (%q, %q), 期望 (%q, A1)", session, payload, key1)
	}

	// ---- 会话 2：复用同一条承载连接 ----
	if _, err := thirdB.Write([]byte("B1")); err != nil {
		t.Fatalf("第三方 B 发送失败: %v", err)
	}
	cr2 := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key2 := "svc-u2/" + cr2.ClientConnID
	if key2 == key1 {
		t.Fatalf("两个会话的配对键不应相同")
	}
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key2,
		PairingToken: "token-2",
		Proto:        "udp",
		Role:         protocol.RoleClient,
	})

	// 新会话先发 session=key2 + MsgBind 帧。
	session, payload = readTunnelFrame(t, dc.conn, 5*time.Second)
	if session != key2 {
		t.Fatalf("新会话报到帧 session = %q, 期望 %q", session, key2)
	}
	bind2 := decodeBindFrame(t, payload)
	if bind2.PairingKey != key2 || bind2.PairingToken != "token-2" {
		t.Fatalf("新会话 Bind 内容异常: %+v", bind2)
	}
	if bind2.Carrier != protocol.CarrierUDPOverTCP {
		t.Fatalf("新会话 Bind.Carrier = %q", bind2.Carrier)
	}

	// 随后是会话 2 的首包。
	session, payload = readTunnelFrame(t, dc.conn, 5*time.Second)
	if session != key2 || string(payload) != "B1" {
		t.Fatalf("会话 2 首帧 = (%q, %q), 期望 (%q, B1)", session, payload, key2)
	}

	// 双向数据：relay → 会话 2 → 第三方 B。
	writeTunnelFrame(t, dc.conn, key2, []byte("for-B"))
	if got := readUDPWithTimeout(t, thirdB, 3*time.Second); string(got) != "for-B" {
		t.Fatalf("第三方 B 收到 %q, 期望 for-B", got)
	}

	// 第三方 A → 会话 1 → relay。
	if _, err := thirdA.Write([]byte("for-A")); err != nil {
		t.Fatalf("第三方 A 发送失败: %v", err)
	}
	session, payload = readTunnelFrame(t, dc.conn, 5*time.Second)
	if session != key1 || string(payload) != "for-A" {
		t.Fatalf("会话 1 数据帧 = (%q, %q), 期望 (%q, for-A)", session, payload, key1)
	}

	// ---- 单会话错误隔离：控制帧 MsgError 只关闭对应会话 ----
	errFrame, err := protocol.EncodeFrame(protocol.MsgError, &protocol.Error{
		Code:    protocol.ErrCodeInvalidToken,
		Message: "令牌无效",
		Detail:  key1,
	})
	if err != nil {
		t.Fatalf("编码 MsgError 失败: %v", err)
	}
	writeTunnelFrame(t, dc.conn, "", errFrame)
	waitFor(t, 3*time.Second, "会话 1 被单独关闭", func() bool { return tc.c.udpSessionCount() == 1 })

	// 会话 2 仍能正常收发。
	writeTunnelFrame(t, dc.conn, key2, []byte("still-B"))
	if got := readUDPWithTimeout(t, thirdB, 3*time.Second); string(got) != "still-B" {
		t.Fatalf("会话 2 应继续可用，收到 %q", got)
	}

	// ---- 承载连接断开：其上会话全部失效；新会话可懒建新连接 ----
	_ = dc.conn.Close()
	waitFor(t, 5*time.Second, "承载断开后会话清理", func() bool { return tc.c.udpSessionCount() == 0 })

	if _, err := thirdB.Write([]byte("after-carrier-down")); err != nil {
		t.Fatalf("第三方 B 发送失败: %v", err)
	}
	cr3 := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key3 := "svc-u2/" + cr3.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key3,
		PairingToken: "token-3",
		Proto:        "udp",
		Role:         protocol.RoleClient,
	})
	dc2 := r.nextDataConn(t, 5*time.Second)
	if dc2.bind.Carrier != protocol.CarrierUDPOverTCP || dc2.bind.PairingKey != key3 {
		t.Fatalf("重建承载连接首帧异常: %+v", dc2.bind)
	}
	bindOK3, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key3})
	if err != nil {
		t.Fatalf("编码 BindOK 失败: %v", err)
	}
	writeTunnelFrame(t, dc2.conn, "", bindOK3)
	session, payload = readTunnelFrame(t, dc2.conn, 5*time.Second)
	if session != key3 || string(payload) != "after-carrier-down" {
		t.Fatalf("新承载上的首帧 = (%q, %q)", session, payload)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestUDPBindErrorClosesSession 覆盖原生 UDP 报到被 relay 拒绝（MsgError）：
// 只关闭该会话，Run 不受影响。
func TestUDPBindErrorClosesSession(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{udpService("svc-ub")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
		withUDP:  true,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 1 * time.Second
	cfg.Client.Services = []config.ClientService{{ID: "svc-ub", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	third := newUDPClient(t, tc.listenerAddr("svc-ub"))
	if _, err := third.Write([]byte("x")); err != nil {
		t.Fatalf("第三方发送失败: %v", err)
	}
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key := "svc-ub/" + cr.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key,
		PairingToken: "bad",
		Proto:        "udp",
		Role:         protocol.RoleClient,
	})

	dg := r.nextDatagram(t, 5*time.Second)
	decodeBindFrame(t, dg.data)
	errFrame, err := protocol.EncodeFrame(protocol.MsgError, &protocol.Error{
		Code:    protocol.ErrCodeInvalidToken,
		Message: "令牌无效",
	})
	if err != nil {
		t.Fatalf("编码 MsgError 失败: %v", err)
	}
	r.udpSend(t, dg.from, errFrame)

	waitFor(t, 5*time.Second, "会话被清理", func() bool { return tc.c.udpSessionCount() == 0 })
	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("报到失败不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}
