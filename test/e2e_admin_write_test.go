package e2e

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// adminHTTPNoRedirect 不跟随重定向，便于断言 303 与 Location。
var adminHTTPNoRedirect = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// adminCSRFToken 从管理页首页抓取一次性 CSRF 令牌。
func adminCSRFToken(t *testing.T, addr string) string {
	t.Helper()
	body := mustAdminGet(t, addr, "/?refresh=0")
	re := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("管理页未找到 CSRF 令牌: %.300q", body)
	}
	return m[1]
}

// adminPostFormNoRedirect 提交表单并返回状态码与 Location（不跟随重定向）。
func adminPostFormNoRedirect(t *testing.T, addr, action string, form url.Values) (int, string) {
	t.Helper()
	if !strings.HasPrefix(action, "/") {
		action = "/" + action
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+action, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("构造 POST 请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := adminHTTPNoRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", action, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// adminWrite 提交一次带 CSRF 的写操作，断言 303 并返回重定向后的提示页面正文。
func adminWrite(t *testing.T, addr, action string, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", adminCSRFToken(t, addr))
	code, location := adminPostFormNoRedirect(t, addr, action, form)
	if code != http.StatusSeeOther {
		t.Fatalf("POST %s 状态码 = %d, want 303（Location=%q）", action, code, location)
	}
	if location == "" {
		t.Fatalf("POST %s 缺少 Location", action)
	}
	body := mustAdminGet(t, addr, strings.TrimPrefix(location, "http://"+addr))
	if !strings.Contains(body, "操作成功") {
		t.Fatalf("写操作后页面缺少成功提示: %.500q", body)
	}
	return body
}

// adminWriteFails 断言一次写操作以 303 重定向到错误提示，并返回页面正文。
func adminWriteFails(t *testing.T, addr, action string, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", adminCSRFToken(t, addr))
	code, location := adminPostFormNoRedirect(t, addr, action, form)
	if code != http.StatusSeeOther {
		t.Fatalf("POST %s 状态码 = %d, want 303", action, code)
	}
	body := mustAdminGet(t, addr, strings.TrimPrefix(location, "http://"+addr))
	if !strings.Contains(body, "操作失败") {
		t.Fatalf("失败写操作后页面缺少失败提示: %.500q", body)
	}
	return body
}

// waitDialEcho 轮询连接本地端口并做一次 echo 往返。
func waitDialEcho(t *testing.T, addr, payload string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			if eerr := echoTextErr(t, conn, payload); eerr == nil {
				_ = conn.Close()
				return
			} else {
				lastErr = eerr
			}
			_ = conn.Close()
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("连接 %s 并 echo 超时: %v", addr, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// echoTextErr 是 echoText 的非 Fatal 版本。
func echoTextErr(t *testing.T, conn net.Conn, payload string) error {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := conn.Write([]byte(payload)); err != nil {
		return err
	}
	buf := make([]byte, len(payload))
	if _, err := readFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != payload {
		return &echoMismatch{got: string(buf), want: payload}
	}
	return nil
}

type echoMismatch struct{ got, want string }

func (e *echoMismatch) Error() string {
	return "echo 响应不匹配: got=" + e.got + " want=" + e.want
}

// readFull 读取恰好 len(buf) 字节。
func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// waitPortClosed 断言端口在超时内变为不可连接。
func waitPortClosed(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("端口 %s 在 %s 内仍未关闭", addr, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readINI 读取配置文件文本。
func readINI(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 ini %s: %v", path, err)
	}
	return string(data)
}

// waitPageContains 轮询管理页直到正文包含 substr。
func waitPageContains(t *testing.T, h *runHandle, addr, path, substr string) {
	t.Helper()
	waitUntil(t, 8*time.Second, "管理页出现 "+substr, []*runHandle{h}, func() bool {
		code, body, err := adminGet(addr, path)
		return err == nil && code == http.StatusOK && strings.Contains(body, substr)
	})
}

// TestE2EAdminAddClientMapping 覆盖 client 管理页新增映射 → 本地端口可访问 →
// ini 出现 service.<id> → 删除后端口关闭、ini 行消失。
func TestE2EAdminAddClientMapping(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
		Manage: relayManage,
	})
	relayH := startRelay(t, relayCfg)

	agentManage := hostPort(freeTCPPort(t))
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    agentManage,
		Protocol:  "tcp",
		Local:     echoAddr,
		Services:  []serverService{{ID: "web", Spec: "tcp:" + echoAddr}},
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default", "web")

	clientManage := hostPort(freeTCPPort(t))
	clientPort := freeTCPPort(t)
	clientCfg, clientPath := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    clientManage,
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientManage, map[string]string{"default": hostPort(clientPort)})

	// 新增映射：本地端口立即可以经 relay/agent 访问真实 TCP 服务。
	newPort := freeTCPPort(t)
	newAddr := hostPort(newPort)
	body := adminWrite(t, clientManage, "/action/client-mapping-save", url.Values{
		"service_id": {"web"},
		"listen":     {newAddr},
	})
	if !strings.Contains(body, "映射 web 已保存") {
		t.Fatalf("页面缺少保存提示: %.300q", body)
	}
	waitDialEcho(t, newAddr, "e2e-admin-client", 5*time.Second)
	content := readINI(t, clientPath)
	if !strings.Contains(content, "service.web = "+newAddr) {
		t.Fatalf("client ini 未写回新映射:\n%s", content)
	}
	waitPageContains(t, clientH, clientManage, "/client?refresh=0", newAddr)

	// 删除映射：端口关闭、ini 行消失。
	adminWrite(t, clientManage, "/action/client-mapping-delete", url.Values{"service_id": {"web"}})
	waitPortClosed(t, newAddr, 5*time.Second)
	content = readINI(t, clientPath)
	if strings.Contains(content, "service.web") {
		t.Fatalf("删除后 client ini 仍有 service.web:\n%s", content)
	}
}

// TestE2EAdminAddAgentService 覆盖 agent 管理页新增服务 → relay 服务表 →
// client 订阅列表 → 通过 client 新映射访问成功。
func TestE2EAdminAddAgentService(t *testing.T) {
	mainEcho := startTCPEchoServer(t)
	apiEcho := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
		Manage: relayManage,
	})
	relayH := startRelay(t, relayCfg)

	agentManage := hostPort(freeTCPPort(t))
	agentCfg, agentPath := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    agentManage,
		Protocol:  "tcp",
		Local:     mainEcho,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientManage := hostPort(freeTCPPort(t))
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    clientManage,
	})
	clientH := startClient(t, clientCfg)
	waitPageContains(t, relayH, relayManage, "/relay?refresh=0", "default")

	// agent 页新增服务 → relay 注册表立即出现。
	adminWrite(t, agentManage, "/action/agent-service-save", url.Values{
		"service_id": {"api"},
		"proto":      {"tcp"},
		"local":      {apiEcho},
	})
	waitServicesRegistered(t, relayH, relayManage, "api", "default")
	if content := readINI(t, agentPath); !strings.Contains(content, "service.api = tcp:"+apiEcho) {
		t.Fatalf("agent ini 未写回新服务:\n%s", content)
	}

	// client 订阅列表出现 api → 新增映射 → 访问成功。
	waitPageContains(t, clientH, clientManage, "/client?refresh=0", "api")

	port := freeTCPPort(t)
	addr := hostPort(port)
	adminWrite(t, clientManage, "/action/client-mapping-save", url.Values{
		"service_id": {"api"},
		"listen":     {addr},
	})
	waitDialEcho(t, addr, "e2e-admin-agent", 5*time.Second)
}

// TestE2EAdminEditRelayTimeout 覆盖 relay 页修改 pair_timeout → 页面、ini、运行时行为。
func TestE2EAdminEditRelayTimeout(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, relayPath := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 10 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	// 用只注册不建立数据面的 fake agent：这样可以制造"只有 client 一侧报到"，
	// 才能观察到 pair_timeout 的运行期变化（真实 agent 会主动完成另一侧报到）。
	startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "timeout-svc",
		Proto:     "tcp",
	})
	waitServicesRegistered(t, relayH, relayManage, "timeout-svc")

	// 无 CSRF 的写请求必须 403。
	code, _ := adminPostFormNoRedirect(t, relayManage, "/action/relay-config", url.Values{
		"key":   {"pair_timeout"},
		"value": {"1s"},
	})
	if code != http.StatusForbidden {
		t.Fatalf("无 CSRF 写操作状态码 = %d, want 403", code)
	}

	// 带 CSRF 修改为 1s：页面显示新值，ini 内容变化。
	body := adminWrite(t, relayManage, "/action/relay-config", url.Values{
		"key":   {"pair_timeout"},
		"value": {"1s"},
	})
	if !strings.Contains(body, "pair_timeout") || !strings.Contains(body, "1s") {
		t.Fatalf("页面未显示新值: %.300q", body)
	}
	content := readINI(t, relayPath)
	if !strings.Contains(content, "pair_timeout = 1s") {
		t.Fatalf("relay ini 未写回新值:\n%s", content)
	}

	// 运行时行为：只报到一侧的 TCP 数据连接应在 ~1s 被关闭（而不是旧的 10s）。
	raw := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)
	result := raw.requestPair("timeout-svc", "runtime-timeout")
	conn := bindRawData(t, relayCfg.Relay.TCPListen, result.PairingKey, result.PairingToken, protocol.RoleClient)
	expectClosedNoBytes(t, conn, 4*time.Second, "pair_timeout 运行期生效")
}

// TestE2EAdminWriteDisabledWhenPublic 覆盖 manage_listen 绑定 0.0.0.0 时只读门禁。
func TestE2EAdminWriteDisabledWhenPublic(t *testing.T) {
	port := freeTCPPort(t)
	relayManage := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	relayCfg, relayPath := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
		Manage: relayManage,
	})
	startRelay(t, relayCfg)

	// 测试通过 127.0.0.1 访问绑定在 0.0.0.0 的管理页。
	adminAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	body := mustAdminGet(t, adminAddr, "/?refresh=0")
	if !strings.Contains(body, "只读模式") || !strings.Contains(body, "manage_listen") {
		t.Fatalf("非回环页面缺少只读横幅: %.300q", body)
	}
	if strings.Contains(body, `action="/action/`) {
		t.Fatalf("非回环页面不应渲染写表单: %.300q", body)
	}
	before := readINI(t, relayPath)

	tok := ""
	code, _ := adminPostFormNoRedirect(t, adminAddr, "/action/relay-config", url.Values{
		"csrf_token": {tok},
		"key":        {"pair_timeout"},
		"value":      {"5s"},
	})
	if code != http.StatusForbidden {
		t.Fatalf("只读模式伪造 CSRF 写操作状态码 = %d, want 403", code)
	}
	code, _ = adminPostFormNoRedirect(t, adminAddr, "/action/relay-config", url.Values{
		"key":   {"pair_timeout"},
		"value": {"5s"},
	})
	if code != http.StatusForbidden {
		t.Fatalf("只读模式无 CSRF 写操作状态码 = %d, want 403", code)
	}
	if strings.Contains(body, `name="csrf_token"`) {
		t.Fatalf("非回环页面不应下发 CSRF 令牌: %.300q", body)
	}
	if got := readINI(t, relayPath); got != before {
		t.Fatalf("只读模式下 ini 被修改:\n%s", got)
	}
}
