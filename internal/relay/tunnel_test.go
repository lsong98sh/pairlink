package relay

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestTunnelHelloAuth 覆盖 Hello 鉴权、版本、角色与 HelloOK 字段。
func TestTunnelHelloAuth(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))

	t.Run("auth_failed", func(t *testing.T) {
		c := dialControl(t, s.tunnelAddr)
		c.sendHello(protocol.RoleAgent, "wrong-token", protocol.ProtocolVersion)
		c.expectError(protocol.ErrCodeAuthFailed)
		expectClosed(t, c.conn, 2*time.Second)
	})

	t.Run("unsupported_version", func(t *testing.T) {
		c := dialControl(t, s.tunnelAddr)
		c.sendHello(protocol.RoleClient, testToken, protocol.ProtocolVersion+1)
		c.expectError(protocol.ErrCodeUnsupportedVersion)
		expectClosed(t, c.conn, 2*time.Second)
	})

	t.Run("bad_role", func(t *testing.T) {
		c := dialControl(t, s.tunnelAddr)
		c.sendHello("bogus", testToken, protocol.ProtocolVersion)
		c.expectError(protocol.ErrCodeBadRequest)
		expectClosed(t, c.conn, 2*time.Second)
	})

	t.Run("agent_ok", func(t *testing.T) {
		c := dialControl(t, s.tunnelAddr)
		ok := c.hello(protocol.RoleAgent, testToken)
		mustPrefix(t, ok.AgentID, "a-", "AgentID")
		if len(ok.AgentID) != 18 {
			t.Fatalf("AgentID 长度 = %d，期望 18", len(ok.AgentID))
		}
		if ok.ClientID != "" {
			t.Fatalf("agent 不应有 ClientID，实际 %q", ok.ClientID)
		}
		if ok.Version != protocol.ProtocolVersion {
			t.Fatalf("Version = %d", ok.Version)
		}
		if ok.UDPMode != protocol.UDPModeTCPTunnel {
			t.Fatalf("未配置 udp_listen 时 udp_mode 应为 tcp_tunnel，实际 %q", ok.UDPMode)
		}
		if ok.TCPDataPort != s.tcpPort || ok.TCPDataPort == 0 {
			t.Fatalf("TCPDataPort = %d，期望 %d", ok.TCPDataPort, s.tcpPort)
		}
		if ok.UDPDataPort != 0 {
			t.Fatalf("UDPDataPort = %d，期望 0", ok.UDPDataPort)
		}
		if ok.HeartbeatSeconds <= 0 {
			t.Fatalf("HeartbeatSeconds = %d", ok.HeartbeatSeconds)
		}
	})

	t.Run("client_ok", func(t *testing.T) {
		c := dialControl(t, s.tunnelAddr)
		ok := c.hello(protocol.RoleClient, testToken)
		mustPrefix(t, ok.ClientID, "c-", "ClientID")
		if len(ok.ClientID) != 18 {
			t.Fatalf("ClientID 长度 = %d，期望 18", len(ok.ClientID))
		}
		if ok.AgentID != "" {
			t.Fatalf("client 不应有 AgentID，实际 %q", ok.AgentID)
		}
	})
}

// TestTunnelHelloEmptyAuthToken 覆盖 auth_token 为空时放行（并 WARN）。
func TestTunnelHelloEmptyAuthToken(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Relay.AuthToken = "" })
	s := startTestServer(t, cfg)
	c := dialControl(t, s.tunnelAddr)
	ok := c.hello(protocol.RoleAgent, "anything")
	mustPrefix(t, ok.AgentID, "a-", "AgentID")
}

// TestTunnelRegisterListSubscribe 覆盖注册、重复注册、列表与订阅推送。
func TestTunnelRegisterListSubscribe(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))

	agent := dialControl(t, s.tunnelAddr)
	agentOK := agent.hello(protocol.RoleAgent, testToken)

	reg := agent.register("svc-a", "tcp")
	if reg.ServiceID != "svc-a" || reg.Proto != "tcp" {
		t.Fatalf("注册响应异常: %+v", reg)
	}
	if reg.UDPMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("ServiceRegistered.UDPMode = %q", reg.UDPMode)
	}

	// 同一 agent 重复注册同一 ID 视为更新，仍返回确认。
	if again := agent.register("svc-a", "tcp"); again.ServiceID != "svc-a" {
		t.Fatalf("重复注册响应异常: %+v", again)
	}

	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	list := client.listServices()
	if len(list.Services) != 1 {
		t.Fatalf("服务数量 = %d，期望 1", len(list.Services))
	}
	svc := list.Services[0]
	if svc.ServiceID != "svc-a" || svc.Proto != "tcp" || !svc.Online || svc.AgentID != agentOK.AgentID {
		t.Fatalf("服务列表项异常: %+v", svc)
	}
	if list.UDPMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("ServiceList.UDPMode = %q", list.UDPMode)
	}
	if list.Revision == 0 {
		t.Fatal("ServiceList.Revision 不应为 0")
	}
	rev0 := list.Revision

	client.send(protocol.MsgSubscribe, &protocol.Subscribe{Enable: true})
	// 订阅成功后 relay 立即下发一次全量 ServiceList 作为推送基线（阶段 4 行为）。
	btyp, bmsg := client.read(3 * time.Second)
	if btyp != protocol.MsgServiceList {
		t.Fatalf("订阅后应收到全量 ServiceList 基线，实际 %s: %v", btyp, bmsg)
	}
	if baseline := bmsg.(*protocol.ServiceList); len(baseline.Services) != 1 || baseline.Revision < rev0 {
		t.Fatalf("订阅基线异常: %+v", baseline)
	}

	agent.register("svc-b", "udp")
	typ, msg := client.read(3 * time.Second)
	if typ != protocol.MsgServiceEvent {
		t.Fatalf("期望 ServiceEvent，实际 %s: %v", typ, msg)
	}
	added := msg.(*protocol.ServiceEvent)
	if added.Event != protocol.EventAdded || added.Service.ServiceID != "svc-b" || added.Service.Proto != "udp" {
		t.Fatalf("added 事件异常: %+v", added)
	}
	if added.Revision <= rev0 {
		t.Fatalf("事件 revision = %d，应大于 %d", added.Revision, rev0)
	}
	lastRev := added.Revision

	agent.register("svc-a", "tcp")
	typ, msg = client.read(3 * time.Second)
	if typ != protocol.MsgServiceEvent {
		t.Fatalf("期望 ServiceEvent，实际 %s: %v", typ, msg)
	}
	updated := msg.(*protocol.ServiceEvent)
	if updated.Event != protocol.EventUpdated || updated.Service.ServiceID != "svc-a" {
		t.Fatalf("updated 事件异常: %+v", updated)
	}
	if updated.Revision <= lastRev {
		t.Fatalf("事件 revision 未单调递增: %d <= %d", updated.Revision, lastRev)
	}

	// 取消订阅后不应再收到推送。
	client.send(protocol.MsgSubscribe, &protocol.Subscribe{Enable: false})
	client.sync()
	agent.register("svc-c", "tcp")
	if typ, msg, err := client.tryRead(400 * time.Millisecond); err == nil {
		t.Fatalf("取消订阅后仍收到推送: %s %v", typ, msg)
	}

	list = client.listServices()
	if len(list.Services) != 3 {
		t.Fatalf("服务数量 = %d，期望 3", len(list.Services))
	}
	if list.Services[0].ServiceID != "svc-a" || list.Services[1].ServiceID != "svc-b" || list.Services[2].ServiceID != "svc-c" {
		t.Fatalf("服务列表未按 service_id 排序: %+v", list.Services)
	}
}

// TestTunnelConnectRequest 覆盖接入请求、错误码与 PairResult/Incoming 一致性。
func TestTunnelConnectRequest(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))

	agent := dialControl(t, s.tunnelAddr)
	agentOK := agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-tcp", "tcp")

	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, result := issuePair(t, agent, client, "svc-tcp", "conn-1")
	if result.PairingKey != "svc-tcp/conn-1" || incoming.PairingKey != result.PairingKey {
		t.Fatalf("配对键不一致: incoming=%q result=%q", incoming.PairingKey, result.PairingKey)
	}
	if result.PairingToken == "" || incoming.PairingToken != result.PairingToken {
		t.Fatal("令牌缺失或两侧不一致")
	}
	if result.Proto != "tcp" || incoming.Proto != "tcp" {
		t.Fatalf("proto 不一致: %q / %q", result.Proto, incoming.Proto)
	}
	if result.Role != protocol.RoleClient || incoming.Role != protocol.RoleAgent {
		t.Fatalf("role 异常: %q / %q", result.Role, incoming.Role)
	}
	if incoming.ClientAddr == "" {
		t.Fatal("Incoming.ClientAddr 不应为空")
	}
	if s.pairs.Len() != 0 {
		t.Fatalf("仅信令阶段不应产生配对占位，pairs.Len = %d", s.pairs.Len())
	}

	// 服务不存在。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "nope",
		ClientConnID: "conn-2",
	})
	client.expectError(protocol.ErrCodeServiceNotFound)

	// client_conn_id 非法。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "svc-tcp",
		ClientConnID: "bad id!",
	})
	client.expectError(protocol.ErrCodeBadRequest)

	// agent 不能发起接入请求，且连接保持可用。
	agent.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "svc-tcp",
		ClientConnID: "conn-3",
	})
	agent.expectError(protocol.ErrCodeBadRequest)
	agent.sync()

	// 服务存在但 agent 不在线。
	if err := s.agents.Remove(agentOK.AgentID); err != nil {
		t.Fatalf("移除 agent 失败: %v", err)
	}
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "svc-tcp",
		ClientConnID: "conn-4",
	})
	client.expectError(protocol.ErrCodeNoAgentOnline)
}

// TestTunnelTLSHelloOK 覆盖 TLS 隧道监听。
func TestTunnelTLSHelloOK(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TLSCert = certPath
		c.Relay.TLSKey = keyPath
	})
	s := startTestServer(t, cfg)

	conn, err := tls.Dial("tcp", s.tunnelAddr, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS 拨号: %v", err)
	}
	defer conn.Close()

	c := &ctrlConn{t: t, conn: conn}
	ok := c.hello(protocol.RoleAgent, testToken)
	mustPrefix(t, ok.AgentID, "a-", "AgentID")
	if state := conn.ConnectionState(); state.Version < tls.VersionTLS12 {
		t.Fatalf("TLS 版本过低: %x", state.Version)
	}
}

// TestTunnelTLSBadCert 覆盖证书加载失败时启动返回错误。
func TestTunnelTLSBadCert(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TLSCert = filepath.Join(dir, "missing-cert.pem")
		c.Relay.TLSKey = filepath.Join(dir, "missing-key.pem")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := newServer(cfg).serve(ctx)
	if err == nil {
		t.Fatal("证书缺失时 serve 应返回错误")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("错误信息应包含 TLS: %v", err)
	}
}
