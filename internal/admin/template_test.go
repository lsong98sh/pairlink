package admin

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// startIndexProbe 构造一个未启动的管理页服务，并注入一个安全页面与写操作，
// 便于直接调用 handleIndex 检查渲染结果。
func startIndexProbe(t *testing.T) *Server {
	t.Helper()
	s := New("127.0.0.1:0", Options{
		Title:   "注入测试",
		Mode:    "relay",
		Version: "v0",
		Logger:  discardLogger(),
	})
	s.opts.Actions = []Action{{Name: "safe-action", Title: "安全操作", Method: http.MethodPost}}
	s.pages = []Page{{
		Path:   "/safe",
		Title:  "安全页",
		Render: func(io.Writer, *http.Request) {},
	}}
	return s
}

// serveAdmin 用服务自己的 mux + 安全响应头处理一次请求，不真正监听端口。
func serveAdmin(s *Server, target string) (int, string) {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.String()
}

// TestIndexEscapesUntrustedStrings 把四类 HTML 注入载荷逐个放进内置首页会渲染
// 的每个外部可控字符串字段，断言响应体不含可执行原文、含转义后的形式。
func TestIndexEscapesUntrustedStrings(t *testing.T) {
	payloads := []string{
		`<script>alert(1)</script>`,
		`"><img src=x onerror=alert(1)>`,
		`' onmouseover='x`,
		`</textarea><script>x</script>`,
	}
	fields := []struct {
		name    string
		set     func(*Server, string) string // 返回请求目标
		urlAttr bool                         // 值只出现在 URL 属性上下文
	}{
		{"title", func(s *Server, p string) string { s.opts.Title = p; return "/" }, false},
		{"mode", func(s *Server, p string) string { s.opts.Mode = p; return "/" }, false},
		{"version", func(s *Server, p string) string { s.opts.Version = p; return "/" }, false},
		{"page-title", func(s *Server, p string) string { s.pages[0].Title = p; return "/" }, false},
		{"page-path", func(s *Server, p string) string { s.pages[0].Path = p; return "/" }, false},
		{"action-name", func(s *Server, p string) string { s.opts.Actions[0].Name = p; return "/" }, true},
		{"action-title", func(s *Server, p string) string { s.opts.Actions[0].Title = p; return "/" }, false},
		{"action-method", func(s *Server, p string) string { s.opts.Actions[0].Method = p; return "/" }, false},
		{"notice", func(_ *Server, p string) string {
			return "/?err=" + url.QueryEscape(p)
		}, false},
	}

	for pi, payload := range payloads {
		for _, field := range fields {
			t.Run(fmt.Sprintf("%s/payload%d", field.name, pi), func(t *testing.T) {
				s := startIndexProbe(t)
				target := field.set(s, payload)
				rec := httptest.NewRecorder()
				s.handleIndex(rec, httptest.NewRequest(http.MethodGet, target, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("状态码 = %d, want 200", rec.Code)
				}
				body := rec.Body.String()
				if strings.Contains(body, payload) {
					t.Fatalf("字段 %s 渲染了未转义的原文 %q:\n%s", field.name, payload, body)
				}
				want := template.HTMLEscapeString(payload)
				if field.urlAttr {
					// URL 属性上下文由 html/template 先做 URL 转义（十六进制小写）。
					want = strings.ToLower((&url.URL{Path: payload}).String())
				}
				if !strings.Contains(body, want) {
					t.Fatalf("字段 %s 缺少转义形式 %q:\n%s", field.name, want, body)
				}
			})
		}
	}
}

// TestIndexBodyKeepsStructure 断言首页主体有真实的表格/表单结构，证明模板输出
// 没有被二次转义。
func TestIndexBodyKeepsStructure(t *testing.T) {
	s := startIndexProbe(t)
	s.AddPage(Page{Path: "/services", Title: "服务列表", Render: func(io.Writer, *http.Request) {}})
	s.opts.Actions = []Action{{Name: "cfg", Title: "修改", Method: http.MethodPost}}
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<section><h2>概览</h2><table>`,
		`<thead>`,
		`<tbody>`,
		`<a href="/services">服务列表</a>`,
		`<form class="inline" method="POST" action="/action/cfg">`,
		`<input type="hidden" name="csrf_token" value="`,
		`<button type="submit" class="danger">修改</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("首页缺少结构片段 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "&lt;table") || strings.Contains(body, "&lt;form") {
		t.Fatalf("首页结构被二次转义:\n%s", body)
	}
}

// TestPageBodyEmbeddedVerbatim 断言 AddPage 注册的页面主体仍以 template.HTML
// 原样嵌入布局（表格结构不被转义破坏）。
func TestPageBodyEmbeddedVerbatim(t *testing.T) {
	s := New("127.0.0.1:0", Options{Title: "主体", Mode: "relay", Version: "v0", Logger: discardLogger()})
	s.AddPage(Page{
		Path:  "/raw",
		Title: "原始主体",
		Render: func(w io.Writer, _ *http.Request) {
			io.WriteString(w, `<section id="raw"><table><thead><tr><th>键</th></tr></thead>`+
				`<tbody><tr><td>值</td></tr></tbody></table></section>`)
		},
	})
	status, body := serveAdmin(s, "/raw")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", status)
	}
	for _, want := range []string{
		`<section id="raw">`, `<table>`, `<thead>`, `<tbody>`, `<th>键</th>`, `<td>值</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面主体缺少结构片段 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "&lt;section") || strings.Contains(body, "&lt;table") {
		t.Fatalf("页面主体被二次转义:\n%s", body)
	}
}

// TestRenderFailureReturns500AndLogs 注入一个执行期报错的布局模板，断言返回
// 500、不写出半截页面，并记录 admin_render_failed 事件。
func TestRenderFailureReturns500AndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	s := New("127.0.0.1:0", Options{Title: "渲染失败", Mode: "relay", Version: "v0", Logger: logger})

	old := layoutTemplate
	layoutTemplate = template.Must(template.New("layout").Funcs(template.FuncMap{
		"boom": func() (string, error) { return "", errors.New("模板函数失败") },
	}).Parse(`{{boom}}`))
	t.Cleanup(func() { layoutTemplate = old })

	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("渲染失败状态码 = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"<!DOCTYPE html>", "<html", "<table>", "<section"} {
		if strings.Contains(body, leak) {
			t.Fatalf("渲染失败不应写出半截页面，发现 %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(logs.String(), "event=admin_render_failed") {
		t.Fatalf("日志缺少 admin_render_failed:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "admin_render_error") {
		t.Fatalf("日志不应再使用旧事件名 admin_render_error:\n%s", logs.String())
	}
}

// TestRefreshUsesShowForms 覆盖 P1-2 的自动刷新默认值，且表单判断来自
// FormReporter，而不是扫描生成后的 HTML。
func TestRefreshUsesShowForms(t *testing.T) {
	readOnly := New("0.0.0.0:0", Options{Title: "只读", Mode: "relay", Version: "v0", Logger: discardLogger()})
	readOnly.AddPage(Page{
		Path:   "/plain",
		Title:  "只读页",
		Render: func(w io.Writer, _ *http.Request) { io.WriteString(w, `<section><p>ok</p></section>`) },
	})
	readOnly.AddPage(Page{
		Path:      "/form",
		Title:     "表单页",
		ShowForms: func(*http.Request) bool { return true },
		Render: func(w io.Writer, _ *http.Request) {
			io.WriteString(w, `<section><form method="post" action="/action/x"><input name="a"></form></section>`)
		},
	})

	checkRefresh := func(target string, want bool, wantText string) {
		t.Helper()
		status, body := serveAdmin(readOnly, target)
		if status != http.StatusOK {
			t.Fatalf("GET %s 状态码 = %d, want 200", target, status)
		}
		got := strings.Contains(body, `http-equiv="refresh"`)
		if got != want {
			t.Fatalf("GET %s 自动刷新 = %v, want %v", target, got, want)
		}
		if !strings.Contains(body, wantText) {
			t.Fatalf("GET %s 顶栏缺少 %q", target, wantText)
		}
	}
	checkRefresh("/plain", true, "自动刷新 开启")
	checkRefresh("/plain?refresh=0", false, "自动刷新 暂停")
	checkRefresh("/form", false, "自动刷新 暂停")
	checkRefresh("/form?refresh=1", true, "自动刷新 开启")

	// 只读首页即使列出了操作（禁用项）也不含表单，应保留默认刷新。
	readOnlyIndex := New("0.0.0.0:0", Options{
		Title:   "只读首页",
		Mode:    "relay",
		Version: "v0",
		Logger:  discardLogger(),
		Actions: []Action{{Name: "cfg", Title: "修改"}},
	})
	status, body := serveAdmin(readOnlyIndex, "/")
	if status != http.StatusOK {
		t.Fatalf("只读首页状态码 = %d", status)
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("只读首页（无表单）应默认开启自动刷新")
	}
	if strings.Contains(body, `action="/action/`) {
		t.Fatalf("只读首页不应渲染写表单:\n%s", body)
	}

	// 可写首页含写表单，默认暂停刷新。
	writableIndex := New("127.0.0.1:0", Options{
		Title:   "可写首页",
		Mode:    "relay",
		Version: "v0",
		Logger:  discardLogger(),
		Actions: []Action{{Name: "cfg", Title: "修改"}},
	})
	status, body = serveAdmin(writableIndex, "/")
	if status != http.StatusOK {
		t.Fatalf("可写首页状态码 = %d", status)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("可写首页（含表单）默认不应自动刷新")
	}
	if !strings.Contains(body, `action="/action/cfg"`) {
		t.Fatalf("可写首页应渲染写表单:\n%s", body)
	}
}

// TestWriteHTMLCompatRefresh 锁定兼容入口 WriteHTML 的默认刷新行为：
// 只读页保持刷新、可写页暂停、?refresh=1 可显式开启。
func TestWriteHTMLCompatRefresh(t *testing.T) {
	body := template.HTML(`<section><p>兼容入口</p></section>`)
	readOnly := New("0.0.0.0:0", Options{Title: "只读", Mode: "relay", Version: "v0", Logger: discardLogger()})
	rec := httptest.NewRecorder()
	readOnly.WriteHTML(rec, httptest.NewRequest(http.MethodGet, "/", nil), body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("只读页经 WriteHTML 应默认开启自动刷新: status=%d", rec.Code)
	}
	writable := New("127.0.0.1:0", Options{Title: "可写", Mode: "relay", Version: "v0", Logger: discardLogger()})
	rec = httptest.NewRecorder()
	writable.WriteHTML(rec, httptest.NewRequest(http.MethodGet, "/?refresh=1", nil), body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("?refresh=1 应显式开启自动刷新: status=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	writable.WriteHTML(rec, httptest.NewRequest(http.MethodGet, "/", nil), body)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("可写页经 WriteHTML 应默认暂停自动刷新: status=%d", rec.Code)
	}
}
