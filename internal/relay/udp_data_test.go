package relay

import (
	"net"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// dialUDPRelay 建立到 relay 原生 UDP 数据面的连接。
func dialUDPRelay(t *testing.T, port int) *net.UDPConn {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("连接 UDP 数据面: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// sendUDPBind 发送一个携带 MsgBind 的报到数据报。
func sendUDPBind(t *testing.T, conn *net.UDPConn, key, token, role string) {
	t.Helper()
	frame, err := protocol.EncodeFrame(protocol.MsgBind, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("编码 UDP Bind: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("发送 UDP Bind: %v", err)
	}
}

// sendUDPRaw 发送一个原始数据报。
func sendUDPRaw(t *testing.T, conn *net.UDPConn, payload []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("发送 UDP 数据: %v", err)
	}
}

// readUDPFrame 读取一个数据报并解析为 PairLink 帧。
func readUDPFrame(t *testing.T, conn *net.UDPConn, timeout time.Duration) (protocol.MessageType, any) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读 UDP 数据报: %v", err)
	}
	typ, payload, err := protocol.ParseFrame(buf[:n])
	if err != nil {
		t.Fatalf("解析 UDP 帧: %v", err)
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		t.Fatalf("解码 UDP 帧: %v", err)
	}
	return typ, msg
}

// readUDPData 读取一个纯数据报。
func readUDPData(t *testing.T, conn *net.UDPConn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读 UDP 数据报: %v", err)
	}
	return append([]byte(nil), buf[:n]...)
}

// expectUDPTimeout 断言在 timeout 内没有收到任何数据报。
func expectUDPTimeout(t *testing.T, conn *net.UDPConn, timeout time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1024)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("本应没有数据报，却收到 %d 字节: %q", n, string(buf[:n]))
	}
}

// TestNativeUDPForwardAndTimeout 覆盖原生 UDP 的报到、映射、双向转发与完整会话超时清理。
func TestNativeUDPForwardAndTimeout(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = "127.0.0.1:0"
		c.Relay.UDPSessionTimeout = 400 * time.Millisecond
	})
	s := startTestServer(t, cfg)
	if s.udpMode != protocol.UDPModeNative {
		t.Fatalf("udp_mode = %q，期望 native", s.udpMode)
	}

	agent := dialControl(t, s.tunnelAddr)
	agentOK := agent.hello(protocol.RoleAgent, testToken)
	if agentOK.UDPMode != protocol.UDPModeNative || agentOK.UDPDataPort != s.udpPort || agentOK.UDPDataPort == 0 {
		t.Fatalf("native 模式 HelloOK 数据面字段异常: %+v", agentOK)
	}
	if reg := agent.register("udp-svc", "udp"); reg.UDPMode != protocol.UDPModeNative {
		t.Fatalf("ServiceRegistered.UDPMode = %q", reg.UDPMode)
	}
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	if list := client.listServices(); list.UDPMode != protocol.UDPModeNative {
		t.Fatalf("ServiceList.UDPMode = %q", list.UDPMode)
	}

	incoming, result := issuePair(t, agent, client, "udp-svc", "conn-udp")
	if result.Proto != "udp" || incoming.Proto != "udp" {
		t.Fatalf("proto 异常: %q / %q", result.Proto, incoming.Proto)
	}

	agentUDP := dialUDPRelay(t, s.udpPort)
	clientUDP := dialUDPRelay(t, s.udpPort)

	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	typ, msg := readUDPFrame(t, agentUDP, 2*time.Second)
	if typ != protocol.MsgBindOK {
		t.Fatalf("期望 BindOK，实际 %s: %v", typ, msg)
	}
	if bindOK := msg.(*protocol.BindOK); bindOK.PairingKey != incoming.PairingKey {
		t.Fatalf("BindOK 配对键 = %q", bindOK.PairingKey)
	}

	sendUDPBind(t, clientUDP, result.PairingKey, result.PairingToken, protocol.RoleClient)
	typ, msg = readUDPFrame(t, clientUDP, 2*time.Second)
	if typ != protocol.MsgBindOK {
		t.Fatalf("期望 BindOK，实际 %s: %v", typ, msg)
	}

	// 双向转发。
	sendUDPRaw(t, agentUDP, []byte("ping"))
	if got := readUDPData(t, clientUDP, 2*time.Second); string(got) != "ping" {
		t.Fatalf("client 收到 %q", string(got))
	}
	sendUDPRaw(t, clientUDP, []byte("pong"))
	if got := readUDPData(t, agentUDP, 2*time.Second); string(got) != "pong" {
		t.Fatalf("agent 收到 %q", string(got))
	}

	key := includingKey(t, result.PairingKey)
	if entry, ok := s.udpTable.LookupByKey(key); !ok || !entry.Complete {
		t.Fatalf("UDP 会话未进入 Full: %+v ok=%v", entry, ok)
	}

	// 完整会话空闲超时清理。
	waitFor(t, 4*time.Second, "UDP 完整会话清理", func() bool {
		_, ok := s.udpTable.LookupByKey(key)
		return !ok
	})

	// 清理后再发数据：未登记端点按 Bind 解析失败，直接丢弃。
	sendUDPRaw(t, agentUDP, []byte("late"))
	expectUDPTimeout(t, clientUDP, 300*time.Millisecond)

	// 已消费令牌不能重新登记。
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	expectUDPTimeout(t, agentUDP, 300*time.Millisecond)
}

// TestNativeUDPHalfPairTimeout 覆盖原生 UDP 半会话超时与令牌复用语义。
func TestNativeUDPHalfPairTimeout(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = "127.0.0.1:0"
		c.Relay.PairTimeout = 300 * time.Millisecond
		c.Relay.UDPSessionTimeout = 5 * time.Second
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, _ := issuePair(t, agent, client, "udp-svc", "conn-half")
	key := includingKey(t, incoming.PairingKey)

	agentUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	if typ, msg := readUDPFrame(t, agentUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("期望 BindOK，实际 %s: %v", typ, msg)
	}

	// 半会话超时清理（令牌未消费）。
	waitFor(t, 4*time.Second, "UDP 半会话清理", func() bool {
		_, ok := s.udpTable.LookupByKey(key)
		return !ok
	})

	// 同一端点重新报到：令牌仍有效，应重新登记并回 BindOK。
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	if typ, msg := readUDPFrame(t, agentUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("半会话超时后重新报到应成功，实际 %s: %v", typ, msg)
	}

	// 错误令牌不应得到 BindOK。
	clientUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, clientUDP, incoming.PairingKey, "wrong-token", protocol.RoleClient)
	expectUDPTimeout(t, clientUDP, 300*time.Millisecond)
}

// includingKey 把配对键字符串解析为 PairingKey。
func includingKey(t *testing.T, text string) pairing.PairingKey {
	t.Helper()
	key, err := pairing.ParsePairingKey(text)
	if err != nil {
		t.Fatalf("解析配对键 %q: %v", text, err)
	}
	return key
}
