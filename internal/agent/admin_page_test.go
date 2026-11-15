package agent

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
)

// agentXSSPayloads 是管理页 XSS 矩阵的四类注入载荷。
var agentXSSPayloads = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`' onmouseover='alert(1)`,
	`</textarea><script>x</script>`,
}

// assertAgentEscaped 断言响应体不含注入原文，且包含模板转义后的形式。
func assertAgentEscaped(t *testing.T, body, payload string) {
	t.Helper()
	if strings.Contains(body, payload) {
		t.Fatalf("响应体包含未转义的注入原文 %q:\n%s", payload, body)
	}
	want := template.HTMLEscapeString(payload)
	if !strings.Contains(body, want) {
		t.Fatalf("响应体缺少转义形式 %q:\n%s", want, body)
	}
}

// agentPageGet 取管理页并返回状态码与正文。
func agentPageGet(t *testing.T, target string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应: %v", target, err)
	}
	return resp.StatusCode, string(body)
}

// TestAgentAdminTemplateEscapesUntrustedStrings 把四类注入载荷放进 agent 页面
// 模板会渲染的每个外部可控字段，断言模板逐字段转义。
func TestAgentAdminTemplateEscapesUntrustedStrings(t *testing.T) {
	for pi, payload := range agentXSSPayloads {
		t.Run(fmt.Sprintf("payload%d", pi), func(t *testing.T) {
			data := agentPageData{
				Writable:  true,
				MainToken: payload,
				NewToken:  payload,
				Services: []agentServiceRow{{
					ID: payload, Proto: payload, Addr: payload, UDPMode: payload, Registered: true,
					Forms: agentServiceForms{Show: true, SaveToken: payload, DeleteToken: payload},
				}},
				Main:    agentMainServiceView{Proto: payload, Local: payload, Exists: true},
				Control: agentControlView{Rows: []agentKVRow{{Key: payload, Value: payload, Mono: true}}},
				Carrier: agentCarrierView{Created: true, Addr: payload, State: payload, Sessions: 1},
				Sessions: []agentSessionRow{{
					Key: payload, Proto: payload, Carrier: payload, Remote: payload,
					Upstream: payload, Ready: true, StartedAt: payload, Age: payload,
				}},
			}
			var buf bytes.Buffer
			if err := agentAdminTemplate.Execute(&buf, data); err != nil {
				t.Fatalf("执行 agent 主体模板: %v", err)
			}
			assertAgentEscaped(t, buf.String(), payload)
		})
	}
}

// TestAgentAdminPageEscapesRelayValues 用假 relay 下发带载荷的 agent_id /
// udp_mode，并检查 notice（?err=）在同一页面上的转义。
func TestAgentAdminPageEscapesRelayValues(t *testing.T) {
	upstream := startTCPEcho(t)

	for _, payload := range agentXSSPayloads {
		t.Run("payload", func(t *testing.T) {
			manageAddr := freeTCPAddr(t)
			f := newFakeRelay(t, relayOptions{
				token:   "test-token",
				agentID: payload,
				udpMode: payload,
			})
			cfg := newTestConfig(f.tunnelAddr(),
				config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: upstream})
			cfg.Common.ManageListen = manageAddr
			// agent_id 不做格式校验，原样进入控制连接快照；
			// udp_mode 会被 normalizeUDPMode 白名单兜底成 native/tcp_tunnel（纵深防御）。
			run := startAgent(t, cfg)

			// 管理页监听与控制连接都在 run 的 goroutine 里建立：只通过 HTTP 探测就绪，
			// 不直接读 a.adminSrv（避免与 startAdmin 的写入构成数据竞争）。
			// 等到 HelloOK 生效（agent_id 出现在页面上）再断言，避免拿到握手前的快照。
			var body string
			waitCond(t, 5*time.Second, func() bool {
				got, ok := tryHTTPGet("http://" + manageAddr + "/status?refresh=0")
				if ok && strings.Contains(got, template.HTMLEscapeString(payload)) {
					body = got
					return true
				}
				return false
			}, "管理页展示 relay 下发的 agent_id")
			assertAgentEscaped(t, body, payload)

			status, notice := agentPageGet(t, "http://"+manageAddr+"/status?err="+url.QueryEscape(payload))
			if status != http.StatusOK {
				t.Fatalf("GET /status?err=... 状态码 = %d", status)
			}
			assertAgentEscaped(t, notice, payload)

			if err := run.stop(t); err != nil {
				t.Fatalf("agent.Run 应返回 nil，实际: %v", err)
			}
			f.close()
		})
	}
}

// TestAgentAdminWriteFormsStructure 断言服务表、主服务表单与新增服务表单的结构、
// 字段名与默认值，并覆盖“含表单页默认不刷新、?refresh=1 才刷新”。
func TestAgentAdminWriteFormsStructure(t *testing.T) {
	upstream := startTCPEcho(t)
	webAddr := startTCPEcho(t)
	manageAddr := freeTCPAddr(t)
	f := newFakeRelay(t, relayOptions{token: "test-token"})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: upstream},
		config.ServerService{ID: "web", Proto: "udp", Addr: webAddr},
	)
	cfg.Common.ManageListen = manageAddr
	run := startAgent(t, cfg)
	waitCond(t, 5*time.Second, func() bool {
		_, ok := tryHTTPGet("http://" + manageAddr + "/status?refresh=0")
		return ok
	}, "管理页可访问")

	status, body := agentPageGet(t, "http://"+manageAddr+"/status?refresh=0")
	if status != http.StatusOK {
		t.Fatalf("GET /status 状态码 = %d", status)
	}

	for _, want := range []string{
		"本机已注册服务", "服务配置（可写回 ini）", "主服务（default）", "新增服务", "控制连接（到 relay）",
		`<form method="post" action="/action/agent-main-save" class="form-inline">`,
		`<input type="hidden" name="csrf_token" value="`,
		`<select name="protocol"><option value="tcp" selected>tcp</option><option value="udp">udp</option></select>`,
		`<input type="text" name="local" value="` + upstream + `" size="28" placeholder="host:port">`,
		`<button type="submit">保存主服务</button>`,
		`<form method="post" action="/action/agent-service-save" class="form-inline">`,
		`<label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label>`,
		`<button type="submit">新增/覆盖</button>`,
		`<form class="inline" method="post" action="/action/agent-service-save">`,
		`<input type="hidden" name="service_id" value="web">`,
		`<select name="proto"><option value="tcp">tcp</option><option value="udp" selected>udp</option></select>`,
		`<input type="text" name="local" value="` + webAddr + `" size="18">`,
		`<form class="inline" method="post" action="/action/agent-service-delete">`,
		`<button type="submit" class="danger">删除</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("agent 管理页缺少结构片段 %q:\n%s", want, body)
		}
	}
	// 主服务（default）在服务表里只读：不得渲染针对 default 的写表单。
	if strings.Contains(body, `<input type="hidden" name="service_id" value="default">`) {
		t.Fatalf("主服务行不应渲染修改/删除表单:\n%s", body)
	}

	// 含表单的可写页默认暂停 1s 自动刷新；?refresh=1 才显式开启。
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("含表单页默认不应带 meta refresh:\n%s", body)
	}
	_, withRefresh := agentPageGet(t, "http://"+manageAddr+"/status?refresh=1")
	if !strings.Contains(withRefresh, `<meta http-equiv="refresh" content="1">`) {
		t.Fatalf("?refresh=1 应显式开启 1s 刷新:\n%s", withRefresh)
	}

	// Page.ShowForms 必须挂上并反映可写模式（布局据此决定默认刷新）。
	// 读 a.adminSrv 必须在 run 退出后：Run 的收尾经 done/errCh 同步，避免竞争。
	if err := run.stop(t); err != nil {
		t.Fatalf("agent.Run 应返回 nil，实际: %v", err)
	}
	pages := run.a.adminSrv.Pages()
	var page *admin.Page
	for i := range pages {
		if pages[i].Path == "/status" {
			page = &pages[i]
			break
		}
	}
	if page == nil {
		t.Fatal("管理页未注册 /status")
	}
	if page.ShowForms == nil {
		t.Fatal("/status 未挂 ShowForms")
	}
	if !page.ShowForms(httptest.NewRequest(http.MethodGet, "/status", nil)) {
		t.Fatal("可写模式 ShowForms 应为 true")
	}
	f.close()
}
