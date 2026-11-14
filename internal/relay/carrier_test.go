package relay

import (
	"net"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// writeTunnel 写一个 UDP over TCP 隧道帧。
func writeTunnel(t *testing.T, conn net.Conn, session string, payload []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteUDPTunnelFrame(conn, session, payload); err != nil {
		t.Fatalf("写隧道帧（session=%q）: %v", session, err)
	}
}

// readTunnel 读一个 UDP over TCP 隧道帧。
func readTunnel(t *testing.T, conn net.Conn, timeout time.Duration) (string, []byte) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	session, payload, err := protocol.ReadUDPTunnelFrame(conn)
	if err != nil {
		t.Fatalf("读隧道帧: %v", err)
	}
	return session, payload
}

// readTunnelControl 读一个控制帧（session=""）并解析其中的 PairLink 帧。
func readTunnelControl(t *testing.T, conn net.Conn, timeout time.Duration) (protocol.MessageType, any) {
	t.Helper()
	session, payload := readTunnel(t, conn, timeout)
	if session != "" {
		t.Fatalf("期望控制帧，实际 session=%q", session)
	}
	typ, raw, err := protocol.ParseFrame(payload)
	if err != nil {
		t.Fatalf("解析控制帧负载: %v", err)
	}
	msg, err := protocol.Decode(typ, raw)
	if err != nil {
		t.Fatalf("解码控制帧负载: %v", err)
	}
	return typ, msg
}

// openCarrier 建立一条承载连接并完成首帧握手，返回连接。
func openCarrier(t *testing.T, s *server, bind *protocol.Bind) net.Conn {
	t.Helper()
	conn := dialRaw(t, "127.0.0.1", s.tcpPort)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgBind, bind); err != nil {
		t.Fatalf("写承载首帧: %v", err)
	}
	typ, msg := readTunnelControl(t, conn, 2*time.Second)
	if typ != protocol.MsgBindOK {
		t.Fatalf("期望承载 BindOK，实际 %s: %v", typ, msg)
	}
	if bindOK, ok := msg.(*protocol.BindOK); !ok || bindOK.PairingKey != bind.PairingKey {
		t.Fatalf("承载 BindOK 内容异常: %+v", msg)
	}
	return conn
}

// encodeBindFrame 把一个 MsgBind 编码为完整 PairLink 帧（用于会话登记）。
func encodeBindFrame(t *testing.T, bind *protocol.Bind) []byte {
	t.Helper()
	frame, err := protocol.EncodeFrame(protocol.MsgBind, bind)
	if err != nil {
		t.Fatalf("编码 MsgBind: %v", err)
	}
	return frame
}

// carrierSessionState 读取承载会话状态（测试内部观察用）。
func carrierSessionState(s *server, key pairing.PairingKey) (agentReady, clientReady, full bool) {
	s.carriers.mu.Lock()
	defer s.carriers.mu.Unlock()
	sess := s.carriers.sessions[key]
	if sess == nil {
		return false, false, false
	}
	return sess.agent != nil, sess.client != nil, sess.full
}

// TestCarrierFirstFrameAuthFailure 覆盖承载连接首帧鉴权失败直接关闭。
func TestCarrierFirstFrameAuthFailure(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, _ := issuePair(t, agent, client, "udp-svc", "conn-bad")

	conn := dialRaw(t, "127.0.0.1", s.tcpPort)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgBind, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: "bad-token",
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	}); err != nil {
		t.Fatalf("写承载首帧: %v", err)
	}
	expectClosed(t, conn, 2*time.Second)
}

// TestCarrierMultiplexAndCleanup 覆盖承载复用、单会话错误、承载断开清理与同连接后续会话。
func TestCarrierMultiplexAndCleanup(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)
	if s.udpMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("udp_mode = %q，期望 tcp_tunnel", s.udpMode)
	}

	agent := dialControl(t, s.tunnelAddr)
	agentOK := agent.hello(protocol.RoleAgent, testToken)
	if agentOK.UDPMode != protocol.UDPModeTCPTunnel || agentOK.UDPDataPort != 0 {
		t.Fatalf("tcp_tunnel 模式 HelloOK 数据面字段异常: %+v", agentOK)
	}
	if reg := agent.register("udp-svc", "udp"); reg.UDPMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("ServiceRegistered.UDPMode = %q", reg.UDPMode)
	}
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	if list := client.listServices(); list.UDPMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("ServiceList.UDPMode = %q", list.UDPMode)
	}

	key1Inc, key1Res := issuePair(t, agent, client, "udp-svc", "conn-1")
	key2Inc, key2Res := issuePair(t, agent, client, "udp-svc", "conn-2")
	key3Inc, key3Res := issuePair(t, agent, client, "udp-svc", "conn-3")
	key1 := includingKey(t, key1Res.PairingKey)
	key2 := includingKey(t, key2Res.PairingKey)
	key3 := includingKey(t, key3Res.PairingKey)

	agentCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey:   key1Inc.PairingKey,
		PairingToken: key1Inc.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	})
	clientCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey:   key1Res.PairingKey,
		PairingToken: key1Res.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	})

	// 第 1 个会话双向转发。
	writeTunnel(t, agentCarrier, key1.String(), []byte("hello-1"))
	session, payload := readTunnel(t, clientCarrier, 2*time.Second)
	if session != key1.String() || string(payload) != "hello-1" {
		t.Fatalf("client 收到 session=%q payload=%q", session, string(payload))
	}
	writeTunnel(t, clientCarrier, key1.String(), []byte("world-1"))
	session, payload = readTunnel(t, agentCarrier, 2*time.Second)
	if session != key1.String() || string(payload) != "world-1" {
		t.Fatalf("agent 收到 session=%q payload=%q", session, string(payload))
	}

	// 第 2 个会话复用同一条承载连接：session=<key> + 完整 MsgBind 帧完成登记。
	writeTunnel(t, agentCarrier, key2.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   key2Inc.PairingKey,
		PairingToken: key2Inc.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	writeTunnel(t, clientCarrier, key2.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   key2Res.PairingKey,
		PairingToken: key2Res.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	waitFor(t, 2*time.Second, "第 2 个会话进入 Full", func() bool {
		agentReady, clientReady, full := carrierSessionState(s, key2)
		return agentReady && clientReady && full
	})
	writeTunnel(t, agentCarrier, key2.String(), []byte("hello-2"))
	if session, payload = readTunnel(t, clientCarrier, 2*time.Second); session != key2.String() || string(payload) != "hello-2" {
		t.Fatalf("第 2 个会话 client 收到 session=%q payload=%q", session, string(payload))
	}
	writeTunnel(t, clientCarrier, key2.String(), []byte("world-2"))
	if session, payload = readTunnel(t, agentCarrier, 2*time.Second); session != key2.String() || string(payload) != "world-2" {
		t.Fatalf("第 2 个会话 agent 收到 session=%q payload=%q", session, string(payload))
	}

	// 第 1 个会话仍然可用。
	writeTunnel(t, agentCarrier, key1.String(), []byte("again-1"))
	if session, payload = readTunnel(t, clientCarrier, 2*time.Second); session != key1.String() || string(payload) != "again-1" {
		t.Fatalf("第 1 个会话受影响: session=%q payload=%q", session, string(payload))
	}

	// 单会话错误：错误令牌只回控制帧 MsgError，不关闭承载连接。
	writeTunnel(t, agentCarrier, key3.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   key3Inc.PairingKey,
		PairingToken: "bad-token",
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	typ, msg := readTunnelControl(t, agentCarrier, 2*time.Second)
	if typ != protocol.MsgError {
		t.Fatalf("期望控制帧 MsgError，实际 %s: %v", typ, msg)
	}
	if protocolErr := msg.(*protocol.Error); protocolErr.Code != protocol.ErrCodeInvalidToken {
		t.Fatalf("错误码 = %q，期望 %q", protocolErr.Code, protocol.ErrCodeInvalidToken)
	} else if protocolErr.Detail != key3.String() {
		t.Fatalf("单会话错误 Detail = %q，期望配对键 %q", protocolErr.Detail, key3.String())
	}

	// 角色不一致同样只影响单会话。
	writeTunnel(t, agentCarrier, key3.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   key3Res.PairingKey,
		PairingToken: key3Res.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	typ, msg = readTunnelControl(t, agentCarrier, 2*time.Second)
	if typ != protocol.MsgError {
		t.Fatalf("期望控制帧 MsgError，实际 %s: %v", typ, msg)
	}
	if protocolErr := msg.(*protocol.Error); protocolErr.Code != protocol.ErrCodeBadRequest {
		t.Fatalf("错误码 = %q，期望 %q", protocolErr.Code, protocol.ErrCodeBadRequest)
	} else if protocolErr.Detail != key3.String() {
		t.Fatalf("单会话错误 Detail = %q，期望配对键 %q", protocolErr.Detail, key3.String())
	}

	// 承载连接未受影响，第 1 个会话仍可用。
	writeTunnel(t, agentCarrier, key1.String(), []byte("after-error"))
	if session, payload = readTunnel(t, clientCarrier, 2*time.Second); session != key1.String() || string(payload) != "after-error" {
		t.Fatalf("单会话错误影响了承载连接: session=%q payload=%q", session, string(payload))
	}

	// 承载连接断开：删除该连接上的全部会话；另一条承载连接保持可用。
	_ = clientCarrier.Close()
	waitFor(t, 4*time.Second, "承载断开后会话清理", func() bool {
		return s.carriers.len() == 0
	})

	// 复用仍然存活的 agent 承载连接登记新会话，证明承载连接没有被连带关闭。
	key4Inc, key4Res := issuePair(t, agent, client, "udp-svc", "conn-4")
	key4 := includingKey(t, key4Res.PairingKey)
	clientCarrier4 := openCarrier(t, s, &protocol.Bind{
		PairingKey:   key4Res.PairingKey,
		PairingToken: key4Res.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	})
	writeTunnel(t, agentCarrier, key4.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   key4Inc.PairingKey,
		PairingToken: key4Inc.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	waitFor(t, 2*time.Second, "新会话进入 Full", func() bool {
		agentReady, clientReady, full := carrierSessionState(s, key4)
		return agentReady && clientReady && full
	})
	writeTunnel(t, agentCarrier, key4.String(), []byte("hello-4"))
	if session, payload = readTunnel(t, clientCarrier4, 2*time.Second); session != key4.String() || string(payload) != "hello-4" {
		t.Fatalf("新会话转发失败: session=%q payload=%q", session, string(payload))
	}
}
