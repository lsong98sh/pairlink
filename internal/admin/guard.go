package admin

import (
	"errors"
	"net"
	"net/http"
	"strings"
)

// 管理页 Host/Origin 校验错误（SEC-M1）。
var (
	// errBadHost 表示 Host 头不是回环名单（127.0.0.0/8、[::1]、localhost）。
	errBadHost = errors.New("admin: Host 头不在允许的回环名单内")
	// errBadOrigin 表示 Origin 与 Host 不匹配（跨站写请求）。
	errBadOrigin = errors.New("admin: Origin 与 Host 不一致")
)

// securityHeaders 给管理页的全部响应加安全响应头（SEC-M1）。
//
// 页面只有内联 CSS、没有任何外部资源与脚本，因此 CSP 用 default-src 'none'
// 并单独允许 style-src 'unsafe-inline'；form-action 'self' 保证表单只能提交回
// 管理页自身，frame-ancestors 'none' + X-Frame-Options: DENY 防点击劫持。
//
// 例外：`/debug/pprof/` 的图表页面依赖内联脚本与 data: 图片，若沿用
// default-src 'none' 会让 pprof 功能整体失效；该端点仅用于本机排障（且只在
// Pprof=true 时挂载），因此单独放宽 script-src/img-src，业务页面策略不变。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		csp := "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'"
		if strings.HasPrefix(r.URL.Path, "/debug/pprof") {
			csp = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'"
		}
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// checkHostOrigin 校验写操作请求的 Host 与 Origin（SEC-M1）。
//
// 目的：防 DNS rebinding 与跨站写。
//   - Host 必须是 127.0.0.0/8（含 [::1] / localhost）加可选端口，与管理页的
//     回环写门禁（IsLoopbackListen）一致；攻击者的域名（Host: evil.example）
//     或 rebinding 后的域名一律拒绝。
//   - Origin 存在时必须等于 http://<同一 Host>（管理页只提供 http）；不存在
//     （curl / 旧浏览器）则只依赖 Host 与 CSRF 一次性令牌。
func checkHostOrigin(r *http.Request) error {
	if r == nil {
		return errBadHost
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return errBadHost
	}
	if !isLoopbackHostPort(host) {
		return errBadHost
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	want := "http://" + strings.ToLower(host)
	if !strings.EqualFold(strings.TrimSuffix(origin, "/"), want) {
		return errBadOrigin
	}
	return nil
}

// isLoopbackHostPort 报告 host[:port] 是否指向回环接口或 localhost。
func isLoopbackHostPort(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
