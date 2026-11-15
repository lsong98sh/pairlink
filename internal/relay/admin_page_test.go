package relay

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/registry"
)

// adminXSSPayloads 是管理页 XSS 矩阵的四类注入载荷。
var adminXSSPayloads = []string{
	`<script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`' onmouseover='alert(1)`,
	`</textarea><script>x</script>`,
}

// assertEscaped 断言响应体不含注入原文，且包含模板转义后的形式。
func assertEscaped(t *testing.T, body, payload string) {
	t.Helper()
	if strings.Contains(body, payload) {
		t.Fatalf("响应体包含未转义的注入原文 %q:\n%s", payload, body)
	}
	want := template.HTMLEscapeString(payload)
	if !strings.Contains(body, want) {
		t.Fatalf("响应体缺少转义形式 %q:\n%s", want, body)
	}
}

// writableAdminProbe 返回一个“可写”的管理页服务实例（回环地址、尚未监听）。
//
// 只用于给页面渲染提供可写判定与一次性令牌，不需要真正 Start。
func writableAdminProbe(s *server) *admin.Server {
	return admin.New("127.0.0.1:0", admin.Options{
		Title:   "PairLink relay 管理页",
		Mode:    "relay",
		Version: "v-test",
		Logger:  s.log,
	})
}

// TestRelayAdminTemplateEscapesUntrustedStrings 把四类注入载荷放进 relay 页面
// 模板会渲染的每个外部可控字段，断言没有 template.HTML 把原文放行。
func TestRelayAdminTemplateEscapesUntrustedStrings(t *testing.T) {
	for _, native := range []bool{true, false} {
		for pi, payload := range adminXSSPayloads {
			name := fmt.Sprintf("tcp_tunnel/payload%d", pi)
			if native {
				name = fmt.Sprintf("native/payload%d", pi)
			}
			t.Run(name, func(t *testing.T) {
				kick := relayKickView{Show: true, Kind: payload, ID: payload, Token: payload}
				data := relayPageData{
					Overview: []relayOverviewRow{{Key: payload, Value: payload}},
					Agents: []relayAgentRow{{
						ID: payload, RemoteAddr: payload, ConnectedAt: payload,
						LastSeen: payload, Services: payload, Kick: kick,
					}},
					Services: []relayServiceRow{{
						ID: payload, Proto: payload, Online: true, AgentID: payload,
						AgentAddr: payload, RegisteredAt: payload, Kick: kick,
					}},
					Pairs: []relayPairRow{{
						Key: payload, AgentArrived: true, Remaining: payload, Kick: kick,
					}},
					UDP: relayUDPSection{
						Native: native,
						Rows: []relayUDPSessionRow{{
							Key: payload, AgentSide: payload, ClientSide: payload, Complete: true,
							CreatedAt: payload, LastSeen: payload, Kick: kick,
						}},
					},
					Config: relayConfigData{Writable: true, Params: []relayConfigParam{{
						Key: payload, Title: payload, Current: payload, Token: payload,
					}}},
				}
				var buf bytes.Buffer
				if err := relayAdminTemplate.Execute(&buf, data); err != nil {
					t.Fatalf("执行 relay 主体模板: %v", err)
				}
				assertEscaped(t, buf.String(), payload)
			})
		}
	}
}

// TestRelayAdminPageEscapesRegistryValues 通过真实状态（服务/agent 注册表）注入
// 载荷，断言装配层只传 string、不会把值包成 template.HTML。
func TestRelayAdminPageEscapesRegistryValues(t *testing.T) {
	s := newServer(testConfig(t, nil))
	s.admin = writableAdminProbe(s)

	for _, payload := range adminXSSPayloads {
		if err := s.services.Register(registry.ServiceEntry{
			ID: payload, Proto: payload, AgentID: payload, AgentAddr: payload, Online: true,
		}); err != nil {
			t.Fatalf("注册服务 %q: %v", payload, err)
		}
		if err := s.agents.Add(&registry.Agent{
			ID: payload, RemoteAddr: payload, Services: []string{payload},
		}); err != nil {
			t.Fatalf("注册 agent %q: %v", payload, err)
		}
	}

	var buf bytes.Buffer
	s.renderAdmin(&buf, nil)
	body := buf.String()
	for _, payload := range adminXSSPayloads {
		assertEscaped(t, body, payload)
	}
}

// TestRelayAdminWriteFormsStructure 断言三处写表单（踢出、五参数配置）的结构、
// 字段名与默认值与旧输出一致。
func TestRelayAdminWriteFormsStructure(t *testing.T) {
	s := newServer(testConfig(t, nil))
	s.admin = writableAdminProbe(s)

	if err := s.agents.Add(&registry.Agent{
		ID: "a-1", RemoteAddr: "127.0.0.1:5001", Services: []string{"svc-a"},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	if err := s.services.Register(registry.ServiceEntry{
		ID: "svc-a", Proto: "tcp", AgentID: "a-1", AgentAddr: "127.0.0.1:5001", Online: true,
	}); err != nil {
		t.Fatalf("注册服务: %v", err)
	}
	key := pairing.PairingKey{ServiceID: "svc-a", ClientConnID: "conn-1"}
	if _, err := s.pairs.Claim(key, pairing.SideAgent, nil); err != pairing.ErrWaiting {
		t.Fatalf("创建配对占位: %v", err)
	}

	var buf bytes.Buffer
	s.renderAdmin(&buf, nil)
	body := buf.String()

	for _, want := range []string{
		`<form class="inline" method="post" action="/action/kick">`,
		`<button type="submit" class="danger">踢出</button>`,
		`<form class="form-inline" method="post" action="/action/relay-config">`,
		`<button type="submit">修改</button>`,
		`<input type="hidden" name="csrf_token" value="`,
		`服务注册表（1）`,
		`在线 agent（1）`,
		`当前配对表（1）`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("relay 页面缺少结构片段 %q:\n%s", want, body)
		}
	}

	// 踢出表单的三段隐藏域必须连续相邻：e2e 用该正则直接解析令牌与 ID。
	kickRe := regexp.MustCompile(
		`<input type="hidden" name="csrf_token" value="([^"]+)"><input type="hidden" name="kind" value="agent"><input type="hidden" name="id" value="a-1">`)
	if !kickRe.MatchString(body) {
		t.Fatalf("踢出表单隐藏域不再是相邻的 csrf_token→kind→id:\n%s", body)
	}

	// 五个白名单参数各自带表单，字段名/默认值逐字一致。
	for _, p := range relayParams {
		keyInput := `<input type="hidden" name="key" value="` + p.Key + `">`
		if !strings.Contains(body, keyInput) {
			t.Fatalf("配置表单缺少 %q", keyInput)
		}
		valueInput := `<input type="text" name="value" value="` + p.Get(s).String() + `" size="12">`
		if !strings.Contains(body, valueInput) {
			t.Fatalf("配置表单默认值不是当前运行期值 %q", valueInput)
		}
	}
}

// TestRelayAdminReadonlyOmitsWriteForms 断言非回环（只读）页面不下发表单/令牌。
func TestRelayAdminReadonlyOmitsWriteForms(t *testing.T) {
	cases := []struct {
		name      string
		makeAdmin func(*server) *admin.Server
	}{
		{"nil-admin", func(*server) *admin.Server { return nil }},
		{"non-loopback", func(s *server) *admin.Server {
			return admin.New("0.0.0.0:0", admin.Options{
				Title: "PairLink relay 管理页", Mode: "relay", Version: "v-test", Logger: s.log,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(testConfig(t, nil))
			s.admin = tc.makeAdmin(s)
			if s.adminWritable() {
				t.Fatal("该场景应判定为只读")
			}
			if err := s.services.Register(registry.ServiceEntry{ID: "svc-a", Proto: "tcp", AgentID: "a-1"}); err != nil {
				t.Fatalf("注册服务: %v", err)
			}
			var buf bytes.Buffer
			s.renderAdmin(&buf, nil)
			body := buf.String()
			if strings.Contains(body, `action="/action/`) {
				t.Fatalf("只读页面渲染了写表单:\n%s", body)
			}
			if strings.Contains(body, `name="csrf_token"`) {
				t.Fatalf("只读页面下发了 CSRF 令牌:\n%s", body)
			}
			if !strings.Contains(body, `只读模式下配置区不可编辑`) {
				t.Fatalf("只读页面缺少配置区只读提示:\n%s", body)
			}
		})
	}
}

// TestRelayAdminNoticeAndErrorsEscape 断言 notice（?ok=/?err=）与写操作错误信息
// 走真实 HTTP 链路时也被转义（错误信息会回显非法的 value）。
func TestRelayAdminNoticeAndErrorsEscape(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Common.ManageListen = "127.0.0.1:0" })
	s := startTestServer(t, cfg)
	if s.admin == nil {
		t.Fatal("管理页未启动")
	}
	addr := s.admin.Addr()

	for _, payload := range adminXSSPayloads {
		status, body := httpGet(t, addr, "/relay?err="+url.QueryEscape(payload))
		if status != http.StatusOK {
			t.Fatalf("GET /relay?err=... 状态码 = %d", status)
		}
		assertEscaped(t, body, payload)

		status, body = httpPostForm(t, addr, "/action/relay-config", url.Values{
			"csrf_token": {s.admin.CSRFToken()},
			"key":        {"pair_timeout"},
			"value":      {payload},
		})
		if status != http.StatusOK {
			t.Fatalf("非法 duration 的 POST 状态码 = %d", status)
		}
		if !strings.Contains(body, "操作失败") {
			t.Fatalf("非法 duration 未给出失败提示:\n%s", body)
		}
		assertEscaped(t, body, payload)
	}
}
