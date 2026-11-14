package relay

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// adminTestServer 启动带管理页的 relay，并返回一组已配对的连接。
func adminTestServer(t *testing.T) (*server, *ctrlConn, *ctrlConn, *protocol.HelloOK, *protocol.PairResult, net.Conn, net.Conn) {
	t.Helper()
	cfg := testConfig(t, func(c *config.Config) {
		c.Common.ManageListen = "127.0.0.1:0"
	})
	s := startTestServer(t, cfg)
	agent := dialControl(t, s.tunnelAddr)
	agentOK := agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-a", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, result := issuePair(t, agent, client, "svc-a", "conn-1")
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
	echoRoundTrip(t, clientData, agentData, "admin-page")
	return s, agent, client, agentOK, result, agentData, clientData
}

// httpGet 执行一次 GET 并返回状态码与正文。
func httpGet(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	return resp.StatusCode, string(body)
}

// httpPostForm 执行一次 POST 表单并返回状态码与正文。
func httpPostForm(t *testing.T, addr, path string, form url.Values) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.PostForm("http://"+addr+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestAdminPageAndKick 覆盖管理页内容、CSRF/方法校验与踢出。
func TestAdminPageAndKick(t *testing.T) {
	s, agent, _, agentOK, result, agentData, clientData := adminTestServer(t)

	if s.admin == nil {
		t.Fatal("管理页未启动")
	}
	adminAddr := s.admin.Addr()

	// 页面内容：服务注册表、在线 agent、配对表、UDP 会话表。
	status, body := httpGet(t, adminAddr, "/relay")
	if status != http.StatusOK {
		t.Fatalf("GET /relay 状态码 = %d，正文: %s", status, body)
	}
	for _, want := range []string{"服务注册表", "在线 agent", "当前配对表", "UDP 会话表", "svc-a", agentOK.AgentID, result.PairingKey} {
		if !strings.Contains(body, want) {
			t.Fatalf("管理页缺少 %q", want)
		}
	}

	// 页面正文只写 <section>，不写 html/head/body。
	var rendered bytes.Buffer
	s.renderAdmin(&rendered, nil)
	if strings.Contains(strings.ToLower(rendered.String()), "<html") ||
		strings.Contains(strings.ToLower(rendered.String()), "<body") ||
		strings.Contains(strings.ToLower(rendered.String()), "<head") {
		t.Fatalf("页面正文不应包含 html/head/body: %s", rendered.String())
	}

	// GET 写操作必须 405。
	if status, _ := httpGet(t, adminAddr, "/action/kick"); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /action/kick 状态码 = %d，期望 405", status)
	}

	// 缺少 CSRF 令牌必须 403。
	if status, _ := httpPostForm(t, adminAddr, "/action/kick", url.Values{
		"kind": {"agent"},
		"id":   {agentOK.AgentID},
	}); status != http.StatusForbidden {
		t.Fatalf("无 CSRF 的 POST 状态码 = %d，期望 403", status)
	}

	// 踢出配对：关闭数据连接，既有转发/清理路径负责收尾。
	status, _ = httpPostForm(t, adminAddr, "/action/kick", url.Values{
		"csrf_token": {s.admin.CSRFToken()},
		"kind":       {"pair"},
		"id":         {result.PairingKey},
	})
	if status != http.StatusOK {
		t.Fatalf("踢出配对状态码 = %d，期望 200", status)
	}
	expectClosed(t, clientData, 3*time.Second)
	expectClosed(t, agentData, 3*time.Second)
	waitFor(t, 3*time.Second, "活动配对清理", func() bool { return len(s.activeSnapshot()) == 0 })

	// 踢出 agent：控制连接收到 MsgKicked 后关闭，注册表随之清理。
	status, _ = httpPostForm(t, adminAddr, "/action/kick", url.Values{
		"csrf_token": {s.admin.CSRFToken()},
		"kind":       {"agent"},
		"id":         {agentOK.AgentID},
	})
	if status != http.StatusOK {
		t.Fatalf("踢出 agent 状态码 = %d，期望 200", status)
	}
	typ, msg := agent.read(2 * time.Second)
	if typ != protocol.MsgKicked {
		t.Fatalf("期望 MsgKicked，实际 %s: %v", typ, msg)
	}
	expectClosed(t, agent.conn, 3*time.Second)
	waitFor(t, 3*time.Second, "agent 清理", func() bool {
		return s.agents.Count() == 0 && s.services.Len() == 0
	})
}

// TestAdminKickUnknown 覆盖踢出不存在目标的 404。
func TestAdminKickUnknown(t *testing.T) {
	s, _, _, _, _, _, _ := adminTestServer(t)
	status, _ := httpPostForm(t, s.admin.Addr(), "/action/kick", url.Values{
		"csrf_token": {s.admin.CSRFToken()},
		"kind":       {"agent"},
		"id":         {"a-does-not-exist"},
	})
	if status != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", status)
	}
}

// TestAdminNativeUDPSessionTable 覆盖 native 模式下管理页的 UDP 会话表。
func TestAdminNativeUDPSessionTable(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = "127.0.0.1:0"
		c.Common.ManageListen = "127.0.0.1:0"
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, result := issuePair(t, agent, client, "udp-svc", "conn-admin-udp")
	agentUDP := dialUDPRelay(t, s.udpPort)
	clientUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	if typ, msg := readUDPFrame(t, agentUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("期望 BindOK，实际 %s: %v", typ, msg)
	}
	sendUDPBind(t, clientUDP, result.PairingKey, result.PairingToken, protocol.RoleClient)
	if typ, msg := readUDPFrame(t, clientUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("期望 BindOK，实际 %s: %v", typ, msg)
	}

	status, body := httpGet(t, s.admin.Addr(), "/relay")
	if status != http.StatusOK {
		t.Fatalf("GET /relay 状态码 = %d", status)
	}
	for _, want := range []string{"UDP 会话表（native", result.PairingKey, "Agent 端点", "Client 端点", "完整"} {
		if !strings.Contains(body, want) {
			t.Fatalf("native 管理页缺少 %q", want)
		}
	}
}
