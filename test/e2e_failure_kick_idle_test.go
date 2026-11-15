package e2e

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var kickFormRe = regexp.MustCompile(
	`<input type="hidden" name="csrf_token" value="([^"]+)"><input type="hidden" name="kind" value="agent"><input type="hidden" name="id" value="([^"]+)">`)

// TestE2EFailureKick 验收项：从 relay 管理页 POST /action/kick 踢出 agent；
// 断言 agent 下线、服务注销、client 访问失败；并验证 GET 405、无 CSRF 403。
func TestE2EFailureKick(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
		IdleTimeout: 20 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	// 踢出前确认链路可用。
	conn, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
	if err != nil {
		t.Fatalf("踢出前连接 client 本地端口: %v", err)
	}
	echoText(t, conn, "before-kick")
	_ = conn.Close()
	waitRelayPairCount(t, relayH, relayManage, 0)

	// 解析管理页上的 CSRF 令牌与 agent ID。
	page := relayPage(t, relayManage)
	m := kickFormRe.FindStringSubmatch(page)
	if len(m) != 3 {
		t.Fatalf("relay 管理页未找到 agent 踢出表单:\n%s", page)
	}
	csrf, agentID := m[1], m[2]
	if !strings.HasPrefix(agentID, "a-") {
		t.Fatalf("解析出的 agent ID = %q，期望 a- 前缀", agentID)
	}
	t.Logf("准备踢出 agent_id=%s", agentID)

	// GET 不允许触发写操作。
	code, body, err := adminGet(relayManage, "/action/kick")
	if err != nil {
		t.Fatalf("GET /action/kick: %v", err)
	}
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /action/kick 状态码 = %d，期望 405（body=%q）", code, body)
	}

	// 无 CSRF 令牌的 POST 必须 403。
	resp, err := http.PostForm("http://"+relayManage+"/action/kick", url.Values{
		"kind": {"agent"},
		"id":   {agentID},
	})
	if err != nil {
		t.Fatalf("POST /action/kick（无 CSRF）: %v", err)
	}
	noCSRFBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 的 POST 状态码 = %d，期望 403（body=%q）", resp.StatusCode, noCSRFBody)
	}

	// 合法 POST 踢出。
	resp, err = http.PostForm("http://"+relayManage+"/action/kick", url.Values{
		"csrf_token": {csrf},
		"kind":       {"agent"},
		"id":         {agentID},
	})
	if err != nil {
		t.Fatalf("POST /action/kick: %v", err)
	}
	kickBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("踢出 POST 状态码 = %d，期望 200（body=%q）", resp.StatusCode, kickBody)
	}
	if !strings.Contains(string(kickBody), "已踢出") {
		t.Fatalf("踢出响应体 = %q，期望包含 已踢出", kickBody)
	}

	// agent Run 应以“被踢出”错误退出（不自动重连）。
	waitUntil(t, 5*time.Second, "agent 被踢出后 Run 返回", []*runHandle{agentH}, func() bool {
		return agentH.exited()
	})
	agentErr := agentH.result()
	if agentErr == nil {
		t.Fatalf("agent 被踢出后 Run 返回 nil，期望错误")
	}
	if !strings.Contains(agentErr.Error(), "踢出") {
		t.Fatalf("agent 被踢出后 Run 错误 = %v，期望包含“踢出”", agentErr)
	}

	waitRelayAgentCount(t, relayH, relayManage, 0)
	waitRelayServiceCount(t, relayH, relayManage, 0)

	// 服务已下线：client 本地端口仍监听，但连接应被关闭而不是被转发。
	conn2, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
	if err != nil {
		t.Fatalf("踢出后连接 client 本地端口: %v", err)
	}
	defer conn2.Close()
	if _, err := conn2.Write([]byte("after-kick")); err != nil {
		t.Fatalf("踢出后写本地连接: %v", err)
	}
	expectClosedNoBytes(t, conn2, 4*time.Second, "踢出后 client 本地访问")

	t.Logf("踢出成功: agent_id=%s，agent Run 错误=%v", agentID, agentErr)
}

// TestE2EFailureIdleTimeout 验收项：relay idle_timeout 调小后，已建立配对在
// 无流量时被关闭，管理页配对表回落为 0。
func TestE2EFailureIdleTimeout(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
		IdleTimeout: 500 * time.Millisecond,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
	if err != nil {
		t.Fatalf("连接 client 本地端口: %v", err)
	}
	defer conn.Close()
	echoText(t, conn, "idle-ping")
	lastActive := time.Now()

	// 不再发送数据：relay 的 idle_timeout=500ms 到期后应关闭数据连接，
	// client 侧对本地连接做 CloseWrite，因此本地 Read 得到 EOF。
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置本地连接读 deadline: %v", err)
	}
	buf := make([]byte, 16)
	n, readErr := conn.Read(buf)
	elapsed := time.Since(lastActive)
	if n != 0 {
		t.Fatalf("空闲超时后仍收到 %d 字节: %q", n, buf[:n])
	}
	if readErr == nil {
		t.Fatalf("空闲超时后连接未关闭（Read 返回 nil）")
	}
	if isTimeoutErr(readErr) {
		t.Fatalf("空闲超时后本地连接未被关闭（等待 5s 超时）")
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("连接关闭过早（%s），idle_timeout=500ms 似乎未生效", elapsed)
	}
	_ = conn.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	waitUntil(t, 8*time.Second, "空闲超时后 agent 活跃数据连接回落", nil, func() bool {
		code, body, err := adminGet(agentCfg.Common.ManageListen, "/status?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前没有活跃配对")
	})
	t.Logf("空闲超时清理: idle_timeout=500ms 实际关闭耗时=%s", elapsed.Round(time.Millisecond))
}
