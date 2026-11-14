package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
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
func httpPost(t *testing.T, rawURL string, values url.Values, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
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
	s := startTestServer(t, Options{Title: "PairLink 测试页", Mode: "relay", Version: "v0.1.0", Logger: discardLogger()})
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
	for _, want := range []string{"PairLink 测试页", "relay", "v0.1.0", "服务列表", "/services", "首页"} {
		if !strings.Contains(body, want) {
			t.Errorf("首页缺少 %q", want)
		}
	}
	if !strings.Contains(body, `<meta charset="utf-8">`) {
		t.Error("页面缺少 utf-8 charset")
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("默认应开启自动刷新")
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

func TestCSRF(t *testing.T) {
	called := 0
	s := startTestServer(t, Options{
		Mode:    "relay",
		Version: "v0.1.0",
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
