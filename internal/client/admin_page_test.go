package client

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
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// clientXSSPayloads 是管理页 XSS 矩阵的四类注入载荷。
var clientXSSPayloads = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`' onmouseover='alert(1)`,
	`</textarea><script>x</script>`,
}

// assertClientEscaped 断言响应体不含注入原文，且包含模板转义后的形式。
func assertClientEscaped(t *testing.T, body, payload string) {
	t.Helper()
	if strings.Contains(body, payload) {
		t.Fatalf("响应体包含未转义的注入原文 %q:\n%s", payload, body)
	}
	want := template.HTMLEscapeString(payload)
	if !strings.Contains(body, want) {
		t.Fatalf("响应体缺少转义形式 %q:\n%s", want, body)
	}
}

// clientPageGet 取管理页并返回状态码与正文。
func clientPageGet(t *testing.T, target string) (int, string) {
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

// TestClientAdminTemplateEscapesUntrustedStrings 把四类注入载荷放进 client 页面
// 模板会渲染的每个外部可控字段，断言模板逐字段转义。
func TestClientAdminTemplateEscapesUntrustedStrings(t *testing.T) {
	for pi, payload := range clientXSSPayloads {
		t.Run(fmt.Sprintf("payload%d", pi), func(t *testing.T) {
			data := clientPageData{
				Writable: true,
				Overview: clientOverview{
					RelayAddr: payload, ClientID: payload, UDPMode: payload, Revision: payload,
					TCPPlane: payload, UDPPlane: payload, SessionText: payload,
				},
				Services: []clientServiceRow{{
					ServiceID: payload, Proto: payload, AgentID: payload, Online: true, Revision: 1,
				}},
				Mappings: []clientMappingRow{{
					ServiceID: payload, Listen: payload, Actual: payload, Proto: payload,
					Forms: clientMappingForms{Show: true, SaveToken: payload, DeleteToken: payload},
				}},
				NewToken: payload,
				Pairs: []clientPairRow{{
					Key: payload, Proto: payload, Remote: payload, Started: payload,
					Status: payload, ToRelay: 1, ToLocal: 2,
				}},
			}
			var buf bytes.Buffer
			if err := clientAdminTemplate.Execute(&buf, data); err != nil {
				t.Fatalf("执行 client 主体模板: %v", err)
			}
			assertClientEscaped(t, buf.String(), payload)
		})
	}
}

// TestClientAdminPageEscapesRelayValues 用假 relay 下发带载荷的 service_id /
// proto / agent_id 与 udp_mode，并检查 notice（?err=）的转义。
func TestClientAdminPageEscapesRelayValues(t *testing.T) {
	for _, payload := range clientXSSPayloads {
		t.Run("payload", func(t *testing.T) {
			manageAddr := freeTCPAddr(t)
			fr := newFakeRelay(t, fakeRelayOptions{
				services: []protocol.ServiceInfo{{ServiceID: payload, Proto: payload, AgentID: payload, Online: true}},
				revision: 1,
				udpMode:  payload,
			})
			cfg := testConfig(fr.addr())
			cfg.Common.ManageListen = manageAddr
			tc := startTestClient(t, cfg)
			waitFor(t, 5*time.Second, "控制面就绪", func() bool {
				_, revision := tc.c.servicesView()
				return revision >= 1 && tc.c.controlConn() != nil
			})

			status, body := clientPageGet(t, "http://"+manageAddr+"/client?refresh=0")
			if status != http.StatusOK {
				t.Fatalf("GET /client 状态码 = %d", status)
			}
			assertClientEscaped(t, body, payload)

			status, notice := clientPageGet(t, "http://"+manageAddr+"/client?err="+url.QueryEscape(payload))
			if status != http.StatusOK {
				t.Fatalf("GET /client?err=... 状态码 = %d", status)
			}
			assertClientEscaped(t, notice, payload)

			if err := tc.stop(); err != nil {
				t.Fatalf("client.Run 应返回 nil，实际: %v", err)
			}
		})
	}
}

// TestClientAdminWriteFormsStructure 断言映射表行表单与新增映射表单的结构、
// 字段名与默认值，并覆盖“含表单页默认不刷新、?refresh=1 才刷新”。
func TestClientAdminWriteFormsStructure(t *testing.T) {
	manageAddr := freeTCPAddr(t)
	fr := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{
			{ServiceID: config.DefaultServiceID, Proto: "tcp", AgentID: "a-1", Online: true},
			{ServiceID: "svc-x", Proto: "tcp", AgentID: "a-1", Online: true},
		},
		revision: 1,
	})
	cfg := testConfig(fr.addr())
	cfg.Common.ManageListen = manageAddr
	cfg.Client.Services = []config.ClientService{
		{ID: config.DefaultServiceID, Listen: "127.0.0.1:0"},
		{ID: "svc-x", Listen: "127.0.0.1:0"},
	}
	tc := startTestClient(t, cfg)
	waitFor(t, 5*time.Second, "控制面与本地监听就绪", func() bool {
		_, revision := tc.c.servicesView()
		return revision >= 1 && tc.c.controlConn() != nil && len(tc.c.mappingViews()) == 2
	})

	status, body := clientPageGet(t, "http://"+manageAddr+"/client?refresh=0")
	if status != http.StatusOK {
		t.Fatalf("GET /client 状态码 = %d", status)
	}

	for _, want := range []string{
		"relay 返回的可用服务列表", "本地端口映射表", "当前活跃配对",
		`<form class="inline" method="post" action="/action/client-mapping-save">`,
		`<input type="hidden" name="csrf_token" value="`,
		`<input type="hidden" name="service_id" value="svc-x">`,
		`<input type="text" name="listen" value="127.0.0.1:0" size="18">`,
		`<button type="submit">修改</button>`,
		`<form class="inline" method="post" action="/action/client-mapping-delete">`,
		`<button type="submit" class="danger">删除</button>`,
		`<form method="post" action="/action/client-mapping-save" class="form-inline">`,
		`<label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label>`,
		`<label>本地监听 <input type="text" name="listen" size="20" placeholder="127.0.0.1:8080"></label>`,
		`<button type="submit">新增映射</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("client 管理页缺少结构片段 %q:\n%s", want, body)
		}
	}
	// default 是 agent 主服务的固定 ID，对客户端来说只是普通映射（ini 里
	// [client] service.default 合法且常见），因此管理页同样允许改/删。
	if !strings.Contains(body, `<input type="hidden" name="service_id" value="default">`) {
		t.Fatalf("default 映射行应渲染修改/删除表单:\n%s", body)
	}

	// 含表单的可写页默认暂停 1s 自动刷新；?refresh=1 才显式开启。
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("含表单页默认不应带 meta refresh:\n%s", body)
	}
	_, withRefresh := clientPageGet(t, "http://"+manageAddr+"/client?refresh=1")
	if !strings.Contains(withRefresh, `<meta http-equiv="refresh" content="1">`) {
		t.Fatalf("?refresh=1 应显式开启 1s 刷新:\n%s", withRefresh)
	}

	// Page.ShowForms 必须挂上并反映可写模式（布局据此决定默认刷新）。
	// 读 c.admin 必须在 Run 退出后：done 通道同步，避免与 startAdmin 的写入竞争。
	if err := tc.stop(); err != nil {
		t.Fatalf("client.Run 应返回 nil，实际: %v", err)
	}
	pages := tc.c.admin.Pages()
	var page *admin.Page
	for i := range pages {
		if pages[i].Path == "/client" {
			page = &pages[i]
			break
		}
	}
	if page == nil {
		t.Fatal("管理页未注册 /client")
	}
	if page.ShowForms == nil {
		t.Fatal("/client 未挂 ShowForms")
	}
	if !page.ShowForms(httptest.NewRequest(http.MethodGet, "/client", nil)) {
		t.Fatal("可写模式 ShowForms 应为 true")
	}
}

// TestClientAdminEmptyMappingEscapedPlaceholder 断言空映射表的提示保留
// “service.<ID>”的转义写法（不会被二次转义成 &amp;lt;）。
func TestClientAdminEmptyMappingEscapedPlaceholder(t *testing.T) {
	manageAddr := freeTCPAddr(t)
	fr := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(fr.addr())
	cfg.Common.ManageListen = manageAddr
	tc := startTestClient(t, cfg)
	waitFor(t, 5*time.Second, "控制面就绪", func() bool { return tc.c.controlConn() != nil })

	status, body := clientPageGet(t, "http://"+manageAddr+"/client?refresh=0")
	if status != http.StatusOK {
		t.Fatalf("GET /client 状态码 = %d", status)
	}
	if !strings.Contains(body, "没有本地监听（[client] 未配置 service.&lt;ID&gt;）") {
		t.Fatalf("空映射提示被改动或二次转义:\n%s", body)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("client.Run 应返回 nil，实际: %v", err)
	}
}
