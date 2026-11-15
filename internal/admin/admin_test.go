package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// discardLogger 返回丢弃全部日志的 logger，保持测试输出干净。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startTestServer 在 127.0.0.1:0 启动管理页，并在用例结束时关闭。
func startTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	s := New("127.0.0.1:0", opts)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = s.Shutdown(shutdownCtx)
	})
	return s
}

// httpGet 发起 GET 请求并返回状态码与响应体。
func httpGet(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, string(body)
}

// httpPost 提交表单（可带额外请求头）。
//
// headers 里的 "Host" 会写入 req.Host（Go 的 Transport 忽略 Header["Host"]），
// 用于构造 Host/Origin 校验用例。
func httpPost(t *testing.T, rawURL string, values url.Values, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestIndexAndPages(t *testing.T) {
	s := startTestServer(t, Options{Title: "PairLink 测试页", Mode: "relay", Version: "v9.9.9", Logger: discardLogger()})
	s.AddPage(Page{
		Path:  "/services",
		Title: "服务列表",
		Render: func(w io.Writer, r *http.Request) {
			io.WriteString(w, `<section><h2>服务</h2><p id="svc">服务 A</p></section>`)
		},
	})
	s.AddPage(Page{Path: "", Title: "首页副本", Render: func(io.Writer, *http.Request) {}})
	s.AddPage(Page{Path: "/empty-render", Title: "空渲染", Render: func(io.Writer, *http.Request) {}})

	if got := s.Addr(); got == "127.0.0.1:0" || !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("Addr() = %q，应返回实际监听地址", got)
	}
	if len(s.Pages()) != 2 {
		t.Fatalf("Pages() 长度 = %d, want 2（'/' 页面应被忽略）", len(s.Pages()))
	}

	status, body := httpGet(t, "http://"+s.Addr()+"/")
	if status != http.StatusOK {
		t.Fatalf("首页状态码 = %d", status)
	}
	for _, want := range []string{"PairLink 测试页", "relay", "v9.9.9", "服务列表", "/services", "首页"} {
		if !strings.Contains(body, want) {
			t.Errorf("首页缺少 %q", want)
		}
	}
	if !strings.Contains(body, `<meta charset="utf-8">`) {
		t.Error("页面缺少 utf-8 charset")
	}
	// P1-2：127.0.0.1 属于回环（Writable=true），可写页默认暂停自动刷新，
	// 避免整页重载清空用户正在编辑的表单；需要刷新时点顶栏按钮（?refresh=1）。
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("可写页默认不应注入自动刷新（P1-2）")
	}
	if !strings.Contains(body, "自动刷新 暂停") {
		t.Error("顶栏应显示自动刷新已暂停")
	}
	// 主动开启自动刷新。
	status, body = httpGet(t, "http://"+s.Addr()+"/?refresh=1")
	if status != http.StatusOK || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("?refresh=1 应开启自动刷新: status=%d", status)
	}

	status, body = httpGet(t, "http://"+s.Addr()+"/services")
	if status != http.StatusOK || !strings.Contains(body, "服务 A") {
		t.Fatalf("页面渲染失败: status=%d body=%s", status, body)
	}

	status, _ = httpGet(t, "http://"+s.Addr()+"/no-such-page")
	if status != http.StatusNotFound {
		t.Fatalf("未知路径状态码 = %d, want 404", status)
	}

	// 关闭自动刷新。
	status, body = httpGet(t, "http://"+s.Addr()+"/?refresh=0")
	if status != http.StatusOK || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("?refresh=0 应关闭自动刷新: status=%d", status)
	}
}

// TestRefreshDefaults 覆盖 P1-2 的自动刷新默认值：
//
//   - 只读页（非回环 manage_listen）：默认 1s 刷新；
//   - 可写页：默认暂停（?refresh=1 可显式开启）；
//   - 页面里含 <form>（即使只读）：默认暂停，避免重载丢输入。
func TestRefreshDefaults(t *testing.T) {
	// 只读：127.0.0.1 但把 writable 关掉（等价于非回环 manage_listen）。
	ro := New("127.0.0.1:0", Options{Title: "只读", Mode: "relay", Version: "v0", Logger: discardLogger()})
	ro.writable = false
	ro.AddPage(Page{Path: "/ro", Title: "只读页", Render: func(w io.Writer, r *http.Request) {
		io.WriteString(w, `<section><p>ok</p></section>`)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	if err := ro.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = ro.Shutdown(sctx)
	})
	status, body := httpGet(t, "http://"+ro.Addr()+"/ro")
	if status != http.StatusOK || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("只读页默认应开启自动刷新: status=%d body=%s", status, body)
	}

	// 可写 + 含表单：默认暂停。
	rw := startTestServer(t, Options{Title: "可写", Mode: "relay", Version: "v0"})
	rw.AddPage(Page{Path: "/form", Title: "写表单", Render: func(w io.Writer, r *http.Request) {
		io.WriteString(w, `<section><form method="post" action="/action/x"><input name="a"></form></section>`)
	}})
	status, body = httpGet(t, "http://"+rw.Addr()+"/form")
	if status != http.StatusOK {
		t.Fatalf("写表单页状态码 = %d", status)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("含写表单的页面默认不应自动刷新（否则编辑内容每秒被清空）")
	}
	status, body = httpGet(t, "http://"+rw.Addr()+"/form?refresh=1")
	if status != http.StatusOK || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("?refresh=1 应显式开启自动刷新: status=%d", status)
	}
}

// TestSecurityHeaders 断言 SEC-M1 的安全响应头出现在所有响应上（含 404）。
func TestSecurityHeaders(t *testing.T) {
	s := startTestServer(t, Options{Title: "headers", Mode: "relay", Version: "v0"})
	base := "http://" + s.Addr()
	for _, path := range []string{"/", "/no-such-page"} {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + path)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		want := map[string]string{
			"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		}
		for name, value := range want {
			if got := resp.Header.Get(name); got != value {
				t.Errorf("%s %s = %q, want %q", path, name, got, value)
			}
		}
	}
}

// TestActionHostOriginCheck 断言 SEC-M1：写操作必须来自回环 Host，且 Origin
// （若存在）必须与 Host 一致；伪造 Host / 恶意 Origin 一律 403。
func TestActionHostOriginCheck(t *testing.T) {
	called := 0
	s := startTestServer(t, Options{
		Mode:    "relay",
		Version: "v0",
		Actions: []Action{{
			Name:  "kick",
			Title: "踢出",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called++
				w.WriteHeader(http.StatusOK)
			},
		}},
	})
	base := "http://" + s.Addr()

	// 伪造 Host（DNS rebinding 的典型请求）：即使 CSRF 令牌正确也必须 403。
	tok := s.CSRFToken()
	status, body := httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}},
		map[string]string{"Host": "evil.example"})
	if status != http.StatusForbidden || !strings.Contains(body, "Host/Origin") {
		t.Fatalf("伪造 Host 应 403（Host/Origin 校验）: status=%d body=%s", status, body)
	}
	if called != 0 {
		t.Fatalf("被拒绝的请求不应执行操作: called=%d", called)
	}

	// 合法 Host + 恶意 Origin（跨站写）：403。
	tok = s.CSRFToken()
	status, _ = httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}},
		map[string]string{"Origin": "http://evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("恶意 Origin 应 403: status=%d", status)
	}
	if called != 0 {
		t.Fatalf("被拒绝的请求不应执行操作: called=%d", called)
	}

	// 合法 Host + 同源 Origin：放行。
	tok = s.CSRFToken()
	status, _ = httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}},
		map[string]string{"Origin": "http://" + s.Addr()})
	if status != http.StatusOK || called != 1 {
		t.Fatalf("同源 Origin 应放行: status=%d called=%d", status, called)
	}

	// localhost 也属于允许的回环名单。
	tok = s.CSRFToken()
	status, _ = httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}},
		map[string]string{"Host": "localhost:" + portOf(s.Addr())})
	if status != http.StatusOK || called != 2 {
		t.Fatalf("localhost Host 应放行: status=%d called=%d", status, called)
	}
}

// portOf 返回 host:port 中的端口部分（测试辅助）。
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func TestCSRF(t *testing.T) {
	called := 0
	s := startTestServer(t, Options{
		Mode:    "relay",
		Version: "v9.9.9",
		Actions: []Action{{
			Name:  "kick",
			Title: "踢出",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called++
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "kicked")
			},
		}},
	})
	base := "http://" + s.Addr()

	// GET 不允许触发写操作。
	if status, _ := httpGet(t, base+"/action/kick"); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET 写操作状态码 = %d, want 405", status)
	}
	// 无令牌。
	if status, _ := httpPost(t, base+"/action/kick", url.Values{}, nil); status != http.StatusForbidden {
		t.Fatalf("无 CSRF 令牌状态码 = %d, want 403", status)
	}
	// 错误令牌。
	status, _ := httpPost(t, base+"/action/kick", url.Values{"csrf_token": {"bad"}}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("错误 CSRF 令牌状态码 = %d, want 403", status)
	}
	// 正确令牌，一次性。
	tok := s.CSRFToken()
	status, _ = httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}}, nil)
	if status != http.StatusOK || called != 1 {
		t.Fatalf("正确 CSRF 令牌应执行操作: status=%d called=%d", status, called)
	}
	status, _ = httpPost(t, base+"/action/kick", url.Values{"csrf_token": {tok}}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("令牌应一次性: status=%d", status)
	}

	// 请求头携带令牌。
	tok2 := s.CSRFToken()
	status, _ = httpPost(t, base+"/action/kick", url.Values{}, map[string]string{"X-CSRF-Token": tok2})
	if status != http.StatusOK || called != 2 {
		t.Fatalf("请求头 CSRF 令牌应生效: status=%d called=%d", status, called)
	}

	// 首页表单应包含 CSRF 隐藏字段与操作按钮。
	_, body := httpGet(t, base+"/")
	if !strings.Contains(body, `name="csrf_token"`) || !strings.Contains(body, "踢出") {
		t.Error("首页缺少 CSRF 隐藏字段或操作按钮")
	}
}

func TestPprof(t *testing.T) {
	s := startTestServer(t, Options{Mode: "relay", Pprof: true})
	status, body := httpGet(t, "http://"+s.Addr()+"/debug/pprof/")
	if status != http.StatusOK {
		t.Fatalf("pprof 状态码 = %d", status)
	}
	if !strings.Contains(body, "goroutine") {
		t.Error("pprof 首页应包含 goroutine 信息")
	}
}

func TestPprofDisabled(t *testing.T) {
	s := startTestServer(t, Options{Mode: "agent"})
	if status, _ := httpGet(t, "http://"+s.Addr()+"/debug/pprof/"); status != http.StatusNotFound {
		t.Fatalf("未开启 pprof 时状态码 = %d, want 404", status)
	}
}

func TestShutdown(t *testing.T) {
	s := New("127.0.0.1:0", Options{Mode: "client"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if err := s.Start(ctx); err != ErrAlreadyStarted {
		t.Fatalf("重复 Start 应返回 ErrAlreadyStarted，实际: %v", err)
	}
	addr := s.Addr()
	if status, _ := httpGet(t, "http://"+addr+"/"); status != http.StatusOK {
		t.Fatalf("启动后首页不可用")
	}
	shutdownCtx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("重复 Shutdown 应幂等: %v", err)
	}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	if _, err := client.Get("http://" + addr + "/"); err == nil {
		t.Fatal("Shutdown 后请求应失败")
	}
}

// startTestServerAt 在指定地址启动管理页（用于回环/非回环门禁用例）。
func startTestServerAt(t *testing.T, addr string, opts Options) *Server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	s := New(addr, opts)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start(%s) 失败: %v", addr, err)
	}
	t.Cleanup(func() {
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = s.Shutdown(shutdownCtx)
	})
	return s
}

// TestIsLoopbackListen 覆盖回环地址判定。
func TestIsLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9000":    true,
		"127.0.0.2:9000":    true,
		"[::1]:9000":        true,
		"localhost:9000":    true,
		"0.0.0.0:9000":      false,
		":9000":             false,
		"[::]:9000":         false,
		"192.168.1.10:9000": false,
		"example.com:9000":  false,
		"":                  false,
	}
	for addr, want := range cases {
		if got := IsLoopbackListen(addr); got != want {
			t.Errorf("IsLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

// TestWritableGateLoopback 覆盖回环绑定可写、CSRF 仍生效。
func TestWritableGateLoopback(t *testing.T) {
	called := 0
	s := startTestServerAt(t, "127.0.0.1:0", Options{
		Mode: "relay",
		Actions: []Action{{
			Name:  "cfg",
			Title: "修改",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called++
				w.WriteHeader(http.StatusOK)
			},
		}},
	})
	if !s.Writable() {
		t.Fatal("127.0.0.1 绑定应可写")
	}
	_, body := httpGet(t, "http://"+s.Addr()+"/")
	if strings.Contains(body, "只读模式") {
		t.Fatalf("回环页面不应出现只读横幅: %s", body)
	}
	if status, _ := httpPost(t, "http://"+s.Addr()+"/action/cfg", url.Values{}, nil); status != http.StatusForbidden {
		t.Fatalf("无 CSRF 应 403，实际 %d", status)
	}
	tok := s.CSRFToken()
	if status, _ := httpPost(t, "http://"+s.Addr()+"/action/cfg", url.Values{"csrf_token": {tok}}, nil); status != http.StatusOK || called != 1 {
		t.Fatalf("回环 + CSRF 应执行: status=%d called=%d", status, called)
	}
}

// TestWritableGateNonLoopback 覆盖非回环绑定：页面横幅 + 所有写 Action 403。
func TestWritableGateNonLoopback(t *testing.T) {
	called := 0
	s := startTestServerAt(t, "0.0.0.0:0", Options{
		Mode: "relay",
		Actions: []Action{{
			Name:  "cfg",
			Title: "修改",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called++
				w.WriteHeader(http.StatusOK)
			},
		}},
	})
	if s.Writable() {
		t.Fatal("0.0.0.0 绑定应只读")
	}
	base := "http://" + s.Addr()
	_, body := httpGet(t, base+"/")
	if !strings.Contains(body, "只读模式") || !strings.Contains(body, "manage_listen") {
		t.Fatalf("非回环页面缺少只读横幅: %s", body)
	}
	if !strings.Contains(body, "写操作已禁用") {
		t.Fatalf("只读横幅应说明写操作已禁用: %s", body)
	}
	if strings.Contains(body, `action="/action/`) {
		t.Fatalf("只读模式下不应渲染写表单: %s", body)
	}
	// 带合法 CSRF 也必须 403。
	tok := s.CSRFToken()
	status, _ := httpPost(t, base+"/action/cfg", url.Values{"csrf_token": {tok}}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("只读模式下写操作应 403，实际 %d", status)
	}
	if called != 0 {
		t.Fatal("只读模式下 handler 不应被调用")
	}
	// GET 仍然是 405（方法校验优先于门禁）。
	if status, _ := httpGet(t, base+"/action/cfg"); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET 写操作应 405，实际 %d", status)
	}
}

// TestOnlyPostFields 覆盖表单字段白名单。
func TestOnlyPostFields(t *testing.T) {
	mk := func(values url.Values) *http.Request {
		req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}
	if !OnlyPostFields(mk(url.Values{"key": {"a"}, "value": {"b"}, "csrf_token": {"t"}}), "key", "value") {
		t.Fatal("白名单内字段应通过")
	}
	if OnlyPostFields(mk(url.Values{"key": {"a"}, "evil": {"b"}}), "key") {
		t.Fatal("未知字段应被拒绝")
	}
}

// TestParsePositiveDuration 覆盖 duration 校验。
func TestParsePositiveDuration(t *testing.T) {
	if d, err := ParsePositiveDuration("30s"); err != nil || d != 30*time.Second {
		t.Fatalf("30s 解析失败: %v %v", d, err)
	}
	for _, bad := range []string{"", "abc", "0s", "-1s", "0", strings.Repeat("1", 65)} {
		if _, err := ParsePositiveDuration(bad); err == nil {
			t.Errorf("ParsePositiveDuration(%q) 应报错", bad)
		}
	}
}

// TestRedirectNoticeAndAudit 覆盖提示条与审计日志。
func TestRedirectNoticeAndAudit(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := startTestServerAt(t, "127.0.0.1:0", Options{Logger: logger})
	req, _ := http.NewRequest(http.MethodGet, "/relay?ok=%E5%B7%B2%E4%BF%9D%E5%AD%98&err=%3Cscript%3E", nil)
	bar := string(NoticeBar(req))
	if !strings.Contains(bar, "操作失败") || strings.Contains(bar, "<script>") {
		t.Fatalf("提示条应优先 err 且转义 HTML: %s", bar)
	}
	s.Audit(req, "relay-config", "[relay] pair_timeout", "10s", "30s")
	logs := buf.String()
	for _, want := range []string{"event=admin_action", "action=relay-config", "target=", "old=10s", "new=30s"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("审计日志缺少 %q: %s", want, logs)
		}
	}
}

// TestRedirectNoticeSeeOther 覆盖 303 重定向与查询参数。
func TestRedirectNoticeSeeOther(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/action/x", nil)
	RedirectNotice(rec, req, "/relay", false, "已更新")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/relay?ok=") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFormatUptime(t *testing.T) {
	tests := map[time.Duration]string{
		0:                "0s",
		5 * time.Second:  "5s",
		65 * time.Second: "1m05s",
		2*time.Hour + 3*time.Minute + 4*time.Second: "2h03m04s",
	}
	for d, want := range tests {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
	if got := formatUptime(-time.Second); got != "0s" {
		t.Errorf("负时长应归零，实际 %q", got)
	}
}

// TestAuditDeniedReason 断言 SEC-M1 的审计不会把 Host/Origin 拒绝误标为只读模式：
// 两种拒绝都记 admin_action_denied，但 reason 必须可区分（排障不误导）。
func TestAuditDeniedReason(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	s := startTestServer(t, Options{
		Mode:    "relay",
		Version: "v0",
		Logger:  log,
		Actions: []Action{{
			Name:    "kick",
			Title:   "踢出",
			Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		}},
	})
	base := "http://" + s.Addr()

	// 伪造 Host：403 + admin_denied + admin_action_denied(reason=host_origin)。
	status, _ := httpPost(t, base+"/action/kick", url.Values{"csrf_token": {s.CSRFToken()}},
		map[string]string{"Host": "evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("伪造 Host 应 403，实际 %d", status)
	}
	logs := buf.String()
	if !strings.Contains(logs, "reason=host_origin") {
		t.Fatalf("Host/Origin 拒绝的审计 reason 应为 host_origin:\n%s", logs)
	}
	if strings.Contains(logs, "reason=read_only") {
		t.Fatalf("可写实例不应出现 reason=read_only:\n%s", logs)
	}

	// 只读实例（非回环监听）：403 + reason=read_only。
	// 127.0.0.1 监听 + writable=false，等价于非回环 manage_listen 的只读门禁。
	ro := New("127.0.0.1:0", Options{
		Mode:    "relay",
		Version: "v0",
		Logger:  log,
		Actions: []Action{{
			Name:    "kick",
			Title:   "踢出",
			Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		}},
	})
	ro.writable = false
	ctx, cancel := context.WithCancel(context.Background())
	if err := ro.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(cancel)
	buf.Reset()
	status, _ = httpPost(t, "http://"+ro.Addr()+"/action/kick", url.Values{"csrf_token": {ro.CSRFToken()}}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("只读实例写操作应 403，实际 %d", status)
	}
	if !strings.Contains(buf.String(), "reason=read_only") {
		t.Fatalf("只读门禁的审计 reason 应为 read_only:\n%s", buf.String())
	}
}

// TestSecurityHeadersPprof 断言 pprof 调试端点仍带 CSP（SEC-M1），但单独放宽
// script-src/img-src，否则 /debug/pprof/ 的图表脚本会被 default-src 'none' 全部拦截。
func TestSecurityHeadersPprof(t *testing.T) {
	s := startTestServer(t, Options{Title: "pprof", Mode: "relay", Version: "v0", Pprof: true})
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + s.Addr() + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/ 失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'unsafe-inline'") || !strings.Contains(csp, "img-src 'self' data:") {
		t.Fatalf("pprof CSP 应放宽内联脚本与图片，实际: %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("pprof 响应也应带 X-Frame-Options: DENY")
	}
}
