package relay

import (
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// nativeUDPServer 启动一个启用原生 UDP 数据面的 relay，并注册好 udp-svc 的配对。
func nativeUDPServer(t *testing.T) (*server, *ctrlConn, *ctrlConn, *protocol.Incoming, *protocol.PairResult) {
	t.Helper()
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = "127.0.0.1:0"
	})
	s := startTestServer(t, cfg)
	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "udp-svc", "conn-bind-dup")
	return s, agent, client, incoming, result
}

// TestNativeUDPBindIdempotent 覆盖 A4：半开状态收到同一端点的重复 MsgBind →
// 幂等，再次回 MsgBindOK（令牌只校验不消费）。
func TestNativeUDPBindIdempotent(t *testing.T) {
	s, _, _, incoming, result := nativeUDPServer(t)

	agentUDP := dialUDPRelay(t, s.udpPort)
	// 第一次报到：回 BindOK。
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	if typ, msg := readUDPFrame(t, agentUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("首次报到期望 BindOK，实际 %s: %v", typ, msg)
	}
	// 同一端点重复报到（模拟 BindOK 丢包后的重发）：幂等，再回一次 BindOK。
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	typ, msg := readUDPFrame(t, agentUDP, 2*time.Second)
	if typ != protocol.MsgBindOK {
		t.Fatalf("重复报到应再次回 BindOK，实际 %s: %v", typ, msg)
	}
	if ok, isOK := msg.(*protocol.BindOK); !isOK || ok.PairingKey != incoming.PairingKey {
		t.Fatalf("重复报到的 BindOK 内容异常: %+v", msg)
	}
	// 会话仍是半开（对端未到），配对键只有一侧。
	if entry, found := s.udpTable.LookupByKey(includingKey(t, incoming.PairingKey)); !found || entry.Complete {
		t.Fatalf("重复报到不应把会话推进为 Full: %+v found=%v", entry, found)
	}

	// 对端到齐 → Full（令牌在此刻消费）。
	clientUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, clientUDP, result.PairingKey, result.PairingToken, protocol.RoleClient)
	if typ, msg := readUDPFrame(t, clientUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("client 报到期望 BindOK，实际 %s: %v", typ, msg)
	}
	if entry, found := s.udpTable.LookupByKey(includingKey(t, result.PairingKey)); !found || !entry.Complete {
		t.Fatalf("会话应进入 Full: %+v found=%v", entry, found)
	}
}

// TestNativeUDPFullDuplicateBindDropped 覆盖 A4：会话 Full 后，来自已知端点的
// 同一配对键 MsgBind 帧必须被丢弃（不得转发），其它数据报照常转发。
func TestNativeUDPFullDuplicateBindDropped(t *testing.T) {
	s, _, _, incoming, result := nativeUDPServer(t)

	agentUDP := dialUDPRelay(t, s.udpPort)
	clientUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	readUDPFrame(t, agentUDP, 2*time.Second)
	sendUDPBind(t, clientUDP, result.PairingKey, result.PairingToken, protocol.RoleClient)
	readUDPFrame(t, clientUDP, 2*time.Second)
	waitFor(t, 2*time.Second, "会话 Full", func() bool {
		entry, ok := s.udpTable.LookupByKey(includingKey(t, result.PairingKey))
		return ok && entry.Complete
	})

	// agent 端点重复发同一配对键的 Bind 帧：必须被丢弃，不能出现在 client 侧。
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	expectUDPTimeout(t, clientUDP, 400*time.Millisecond)

	// 普通数据报照常转发（不做内容解析）。
	sendUDPRaw(t, agentUDP, []byte("ping"))
	if got := readUDPData(t, clientUDP, 2*time.Second); string(got) != "ping" {
		t.Fatalf("client 收到 %q，期望 ping", string(got))
	}
	// 反向同样可用。
	sendUDPRaw(t, clientUDP, []byte("pong"))
	if got := readUDPData(t, agentUDP, 2*time.Second); string(got) != "pong" {
		t.Fatalf("agent 收到 %q，期望 pong", string(got))
	}

	// 看起来像 Bind 但配对键不同（另一会话）：按普通数据报转发，不做误判丢弃。
	sendUDPBind(t, agentUDP, "other-svc/another-conn", "whatever-token", protocol.RoleAgent)
	if got := readUDPData(t, clientUDP, 2*time.Second); len(got) == 0 {
		t.Fatal("其它配对键的 Bind 形数据报应照常转发")
	}
}
