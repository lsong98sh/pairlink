// Package admin 提供三种模式复用的管理页框架。
//
// 第一版管理页只读：唯一的写操作通过 Action 注册（默认 POST + 一次性 CSRF 令牌），
// 且禁止用 GET 触发写操作。页面使用内联 CSS，不引用任何外部资源，便于内网/隔离环境使用。
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync"
	"time"
)

// ErrAlreadyStarted 表示 Start 被重复调用。
var ErrAlreadyStarted = errors.New("admin: 管理页已启动")

const (
	// csrfTokenTTL 是一次性 CSRF 令牌的有效期。
	csrfTokenTTL = 10 * time.Minute
	// maxCSRFTokens 是 CSRF 令牌表上限，防止页面反复刷新导致内存增长。
	maxCSRFTokens = 4096
	// shutdownTimeout 是 Shutdown 的默认超时。
	shutdownTimeout = 5 * time.Second
)

// Action 是一个写操作。默认 Method 为 POST；GET 等其它方法一律返回 405。
type Action struct {
	// Name 是 URL 片段，实际路径为 /action/<Name>。
	Name string
	// Title 是页面上显示的中文标题。
	Title string
	// Method 是 HTTP 方法，空值表示 POST。
	Method string
	// Handler 是实际处理函数；CSRF 校验由框架完成，Handler 无需重复校验。
	Handler func(http.ResponseWriter, *http.Request)
}

// Page 是一个只读页面。
type Page struct {
	// Path 是 URL 路径，必须以 '/' 开头。
	Path string
	// Title 是导航与页面标题。
	Title string
	// Render 负责写出页面主体 HTML（会被套入统一布局）。
	Render func(w io.Writer, r *http.Request)
}

// Options 是管理页构造参数。
type Options struct {
	// Title 是管理页标题，空值为 "PairLink 管理页"。
	Title string
	// Mode 是模式名（relay / agent / client），显示在顶栏。
	Mode string
	// Version 是版本号，显示在顶栏。
	Version string
	// Logger 是日志器；nil 时使用 slog.Default()。
	Logger *slog.Logger
	// Pprof 为 true 时在管理端口挂载 /debug/pprof/。
	Pprof bool
	// Actions 是写操作列表，构造时注册到 /action/<Name>。
	Actions []Action
}

// Server 是管理页 HTTP 服务。
type Server struct {
	addr string
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux

	startedAt time.Time

	mu       sync.Mutex
	pages    []Page
	actions  map[string]bool
	listener net.Listener
	httpSrv  *http.Server

	csrfMu     sync.Mutex
	csrfTokens map[string]time.Time
}

// New 创建管理页服务。addr 形如 "127.0.0.1:9000"，测试可用 "127.0.0.1:0"。
func New(addr string, opts Options) *Server {
	if opts.Title == "" {
		opts.Title = "PairLink 管理页"
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		addr:       addr,
		opts:       opts,
		log:        log,
		mux:        http.NewServeMux(),
		startedAt:  time.Now(),
		actions:    make(map[string]bool),
		csrfTokens: make(map[string]time.Time),
	}

	s.mux.HandleFunc("/", s.handleIndex)
	for _, a := range opts.Actions {
		s.registerAction(a)
	}
	if opts.Pprof {
		s.mountPprof()
	}
	return s
}

// mountPprof 在管理端口挂载 pprof；使用独立 mux，不污染 DefaultServeMux。
func (s *Server) mountPprof() {
	s.mux.HandleFunc("/debug/pprof/", pprof.Index)
	s.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	s.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

// registerAction 注册一个 Action 路由。
func (s *Server) registerAction(a Action) {
	if a.Name == "" || a.Handler == nil || strings.Contains(a.Name, "/") {
		s.log.Warn("忽略无效的管理页操作", "event", "admin_action_invalid", "action", a.Name)
		return
	}
	s.mu.Lock()
	if s.actions[a.Name] {
		s.mu.Unlock()
		s.log.Warn("忽略重复的管理页操作", "event", "admin_action_duplicate", "action", a.Name)
		return
	}
	s.actions[a.Name] = true
	s.mu.Unlock()

	method := a.Method
	if method == "" {
		method = http.MethodPost
	}
	action := a
	pattern := "/action/" + action.Name
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, fmt.Sprintf("405 方法不允许：该操作必须使用 %s", method), http.StatusMethodNotAllowed)
			return
		}
		if !s.consumeCSRF(r) {
			http.Error(w, "403 CSRF 校验失败或令牌已过期，请刷新页面重试", http.StatusForbidden)
			return
		}
		action.Handler(w, r)
	})
}

// AddPage 注册一个只读页面。必须在 Start 之前调用（内部使用 http.ServeMux）。
//
// "/" 保留给内置首页，重复注册会被忽略。
func (s *Server) AddPage(p Page) {
	if p.Render == nil {
		return
	}
	if p.Path == "" {
		p.Path = "/"
	}
	if !strings.HasPrefix(p.Path, "/") {
		p.Path = "/" + p.Path
	}
	if p.Path == "/" {
		s.log.Warn("内置首页已占用 '/'，忽略同名页面", "event", "admin_page_ignored", "title", p.Title)
		return
	}
	if p.Title == "" {
		p.Title = p.Path
	}
	page := p
	s.mu.Lock()
	s.pages = append(s.pages, p)
	s.mu.Unlock()

	s.mux.HandleFunc(page.Path, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		page.Render(&buf, r)
		s.WriteHTML(w, r, template.HTML(buf.String()))
	})
}

// Pages 返回已注册页面的副本。
func (s *Server) Pages() []Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Page(nil), s.pages...)
}

// Start 同步完成监听并返回，HTTP 服务在后台 goroutine 中运行；
// ctx 取消时优雅关闭（超时 shutdownTimeout）。
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.httpSrv != nil {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("admin: 监听 %s: %w", s.addr, err)
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.listener = ln
	s.httpSrv = srv
	addr := ln.Addr().String()
	s.mu.Unlock()

	s.log.Info("管理页已启动", "event", "admin_start", "manage_listen", addr, "pprof", s.opts.Pprof)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("管理页服务异常退出", "event", "admin_serve_error", "err", err)
		}
	}()

	if ctx != nil {
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := s.Shutdown(shutdownCtx); err != nil {
				s.log.Warn("管理页关闭不干净", "event", "admin_shutdown", "err", err)
			}
		}()
	}
	return nil
}

// Addr 返回实际监听地址；未启动时返回配置的地址。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Shutdown 优雅关闭管理页；未启动时返回 nil（幂等）。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
	}
	err := srv.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin: 关闭管理页: %w", err)
	}
	return nil
}

// Handler 返回管理页的 http.Handler，便于测试或挂载到已有服务。
func (s *Server) Handler() http.Handler { return s.mux }

// CSRFToken 生成一个一次性 CSRF 令牌（随页面渲染发放，使用后立即失效）。
//
// 页面表单需携带 <input type="hidden" name="csrf_token" value="...">；
// 校验时也接受请求头 X-CSRF-Token。
func (s *Server) CSRFToken() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 失败极其罕见；退化为时间戳 + 计数器组合，仍然不可预测性不足，
		// 但校验失败只会拒绝请求，不会放行。
		raw = []byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.startedAt.UnixNano()))
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	expireAt := now.Add(csrfTokenTTL)

	s.csrfMu.Lock()
	for t, exp := range s.csrfTokens {
		if !now.Before(exp) {
			delete(s.csrfTokens, t)
			continue
		}
		if len(s.csrfTokens) >= maxCSRFTokens {
			delete(s.csrfTokens, t)
		}
	}
	s.csrfTokens[tok] = expireAt
	s.csrfMu.Unlock()
	return tok
}

// CSRFInput 返回可直接插入表单的隐藏输入框。
func (s *Server) CSRFInput() template.HTML {
	return template.HTML(`<input type="hidden" name="csrf_token" value="` + template.HTMLEscapeString(s.CSRFToken()) + `">`)
}

// consumeCSRF 校验并消费一次性 CSRF 令牌。
func (s *Server) consumeCSRF(r *http.Request) bool {
	tok := r.PostFormValue("csrf_token")
	if tok == "" {
		tok = r.Header.Get("X-CSRF-Token")
	}
	if tok == "" {
		return false
	}
	now := time.Now()
	s.csrfMu.Lock()
	exp, ok := s.csrfTokens[tok]
	if ok {
		delete(s.csrfTokens, tok)
	}
	s.csrfMu.Unlock()
	if !ok || !now.Before(exp) {
		return false
	}
	return true
}
