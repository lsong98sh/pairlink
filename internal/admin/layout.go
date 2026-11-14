package admin

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"
)

// navItem 是顶栏导航项。
type navItem struct {
	Path  string
	Title string
}

// layoutData 是统一布局的渲染数据。
type layoutData struct {
	Title     string
	Mode      string
	Version   string
	Uptime    string
	Now       string
	RefreshOn bool
	ToggleURL string
	Writable  bool
	Notice    template.HTML
	Nav       []navItem
	Body      template.HTML
}

// layoutCSS 是内联样式，不引用任何外部资源。
const layoutCSS = `
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{margin:0;font:14px/1.6 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:#f5f6f8;color:#1f2329}
header{display:flex;flex-wrap:wrap;gap:12px;justify-content:space-between;align-items:center;
  padding:10px 18px;background:#1f2937;color:#f9fafb}
header a{color:#93c5fd}
.brand{font-weight:700;font-size:16px;letter-spacing:.5px}
.badge{display:inline-block;padding:1px 8px;border-radius:10px;background:#2563eb;font-size:12px;color:#fff;margin-left:6px}
.dim{color:#6b7280}
header .dim{color:#cbd5e1}
nav{display:flex;flex-wrap:wrap;gap:6px;padding:8px 18px;background:#e5e7eb}
nav a{color:#1f2937;text-decoration:none;padding:3px 10px;border-radius:6px}
nav a:hover{background:#d1d5db}
main{padding:18px;max-width:1200px}
section{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px 16px;margin-bottom:16px}
h2{margin:0 0 10px;font-size:16px}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #eef0f3;vertical-align:top}
th{background:#f9fafb;font-weight:600;white-space:nowrap}
td.mono,code{font-family:ui-monospace,Consolas,"Courier New",monospace;font-size:12px}
.empty{color:#9ca3af;padding:6px 0}
form.inline{display:inline;margin:0}
button{cursor:pointer;border:1px solid #d1d5db;background:#fff;border-radius:6px;padding:3px 10px;font-size:13px}
button:hover{background:#f3f4f6}
button.danger{border-color:#dc2626;color:#dc2626}
button.danger:hover{background:#fef2f2}
.tag{display:inline-block;padding:0 6px;border-radius:4px;background:#eef2ff;color:#3730a3;font-size:12px}
.notice{margin:0 0 14px;padding:8px 12px;border-radius:6px;border:1px solid transparent;font-size:13px}
.notice.ok{background:#ecfdf5;border-color:#6ee7b7;color:#065f46}
.notice.err{background:#fef2f2;border-color:#fca5a5;color:#991b1b}
.banner{margin:0 0 14px;padding:10px 14px;border-radius:6px;font-size:13px;font-weight:600}
.banner.readonly{background:#fef2f2;border:1px solid #dc2626;color:#991b1b}
.form-inline{display:inline-flex;gap:4px;align-items:center;margin:0}
.form-inline input[type=text]{width:150px}
input[type=text],input[type=password],select{border:1px solid #d1d5db;border-radius:6px;padding:3px 6px;font-size:13px}
table form{margin:0}
footer{padding:10px 18px;color:#9ca3af;font-size:12px}
@media (prefers-color-scheme:dark){
 body{background:#111827;color:#e5e7eb}
 section{background:#1f2937;border-color:#374151}
 th{background:#111827}
 th,td{border-color:#374151}
 nav{background:#1f2937}
 nav a{color:#e5e7eb}
 nav a:hover{background:#374151}
 .notice.ok{background:#064e3b;border-color:#047857;color:#d1fae5}
 .notice.err{background:#7f1d1d;border-color:#b91c1c;color:#fee2e2}
 .banner.readonly{background:#7f1d1d;border-color:#dc2626;color:#fee2e2}
 input[type=text],input[type=password],select{background:#111827;color:#e5e7eb;border-color:#4b5563}
}
`

// layoutHTML 是页面骨架：charset 固定 utf-8，自动刷新通过 meta 实现（可关闭）。
const layoutHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
{{if .RefreshOn}}<meta http-equiv="refresh" content="1">{{end}}
<style>` + layoutCSS + `</style>
</head>
<body>
<header>
  <div><span class="brand">PairLink</span><span class="badge">{{.Mode}}</span>
    <span class="dim">{{.Version}}</span></div>
  <div class="dim">运行时长 {{.Uptime}} · 服务器时间 {{.Now}} · 自动刷新 {{if .RefreshOn}}开启{{else}}暂停{{end}}
    · <a href="{{.ToggleURL}}">{{if .RefreshOn}}暂停自动刷新{{else}}开启自动刷新{{end}}</a></div>
</header>
{{if .Nav}}<nav>{{range .Nav}}<a href="{{.Path}}">{{.Title}}</a>{{end}}</nav>{{end}}
<main>
{{if not .Writable}}<div class="banner readonly">只读模式：manage_listen 绑定在非回环地址，出于安全考虑写操作已禁用</div>{{end}}
{{.Notice}}
{{.Body}}
</main>
<footer>PairLink 管理页{{if .Writable}} · 写操作会先改运行时并写回 ini（保留注释与格式）{{else}}（只读） · 配置修改请编辑 ini 后重启进程{{end}}</footer>
</body>
</html>
`

// layoutTemplate 是预编译的页面骨架。
var layoutTemplate = template.Must(template.New("layout").Parse(layoutHTML))

// WriteHTML 把页面主体套入统一布局后写出。
//
// 页面渲染函数应当只写主体 HTML（如 <section>…），布局负责 charset、样式、
// 顶栏（mode / version / 运行时长）与 1s 自动刷新开关。
func (s *Server) WriteHTML(w http.ResponseWriter, r *http.Request, pageBody template.HTML) {
	data := layoutData{
		Title:     s.opts.Title,
		Mode:      s.opts.Mode,
		Version:   s.opts.Version,
		Uptime:    formatUptime(time.Since(s.startedAt)),
		Now:       time.Now().Format("2006-01-02 15:04:05"),
		RefreshOn: autoRefresh(r),
		ToggleURL: toggleRefreshURL(r),
		Writable:  s.writable,
		Notice:    NoticeBar(r),
		Nav:       s.navItems(),
		Body:      pageBody,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := layoutTemplate.Execute(w, data); err != nil {
		s.log.Warn("管理页渲染失败", "event", "admin_render_error", "path", r.URL.Path, "err", err)
	}
}

// navItems 返回顶栏导航（首页 + 已注册页面）。
func (s *Server) navItems() []navItem {
	pages := s.Pages()
	items := make([]navItem, 0, len(pages)+1)
	items = append(items, navItem{Path: "/", Title: "首页"})
	for _, p := range pages {
		if p.Path == "/" {
			continue
		}
		items = append(items, navItem{Path: p.Path, Title: p.Title})
	}
	return items
}

// autoRefresh 报告当前请求是否启用自动刷新（?refresh=0 关闭）。
func autoRefresh(r *http.Request) bool {
	if r == nil {
		return true
	}
	return r.URL.Query().Get("refresh") != "0"
}

// toggleRefreshURL 生成切换自动刷新的 URL（保留其它查询参数）。
func toggleRefreshURL(r *http.Request) string {
	if r == nil {
		return "/?refresh=0"
	}
	u := url.URL{Path: r.URL.Path}
	q := r.URL.Query()
	if autoRefresh(r) {
		q.Set("refresh", "0")
	} else {
		q.Set("refresh", "1")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// formatUptime 把运行时长格式化为人可读字符串（秒级精度）。
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}
