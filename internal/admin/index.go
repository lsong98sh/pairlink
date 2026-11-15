package admin

import (
	"bytes"
	"html/template"
	"net/http"
	"time"
)

// indexPageData 是内置首页的渲染数据。
//
// 所有字段都是普通 string/bool（外部可控值也只以 string 传入），由模板自动
// 转义；首页不携带任何 template.HTML，页面主体在渲染完成后才作为整体嵌入布局。
type indexPageData struct {
	Mode     string
	Version  string
	Uptime   string
	Writable bool
	Pages    []indexPageItem
	Actions  []indexActionItem

	// showForms 表示首页是否真的渲染了写表单（只读模式只列禁用项，不含表单）。
	showForms bool
}

// ShowForms 报告首页是否含表单（供布局决定默认自动刷新）。
func (d indexPageData) ShowForms() bool { return d.showForms }

// indexPageItem 是“页面”表格的一行。
type indexPageItem struct {
	Title string
	Path  string
}

// indexActionItem 是“操作”区的一个写操作；Token 仅在可写模式下生成。
type indexActionItem struct {
	Title  string
	Name   string
	Method string
	Token  string
}

// indexHTML 是内置首页的主体模板；只输出 <section>，由布局套统一骨架。
const indexHTML = `<section><h2>概览</h2><table><tbody>
<tr><th style="width:120px">模式</th><td>{{.Mode}}</td></tr>
<tr><th style="width:120px">版本</th><td>{{.Version}}</td></tr>
<tr><th style="width:120px">运行时长</th><td>{{.Uptime}}</td></tr>
<tr><th style="width:120px">自动刷新</th><td>开启（可通过右上角链接暂停）</td></tr>
{{- if .Writable}}
<tr><th style="width:120px">配置</th><td>可写：写操作会改运行时并写回该 ini（注释/排版可能归一），原子替换且权限不变；可改字段见各模式页面</td></tr>
{{- else}}
<tr><th style="width:120px">配置</th><td>只读：manage_listen 绑定在非回环地址，写操作已禁用</td></tr>
{{- end}}
</tbody></table></section>
<section><h2>页面</h2>
{{- if .Pages}}
<table><thead><tr><th>页面</th><th>路径</th></tr></thead><tbody>
{{- range .Pages}}
<tr><td><a href="{{.Path}}">{{.Title}}</a></td><td class="mono">{{.Path}}</td></tr>
{{- end}}
</tbody></table>
{{- else}}
<p class="empty">暂无页面（阶段2 的 relay / agent / client 会注册各自页面）</p>
{{- end}}
</section>
{{- if .Actions}}
<section><h2>操作</h2>
{{- if .Writable}}
<p class="dim">写操作需要一次性 CSRF 令牌，页面刷新后令牌会更新。</p>
{{- range .Actions}}
<form class="inline" method="{{.Method}}" action="/action/{{.Name}}"><input type="hidden" name="csrf_token" value="{{.Token}}"><button type="submit" class="danger">{{.Title}}</button></form>
{{- end}}
{{- else}}
<p class="dim">只读模式：写操作已禁用（HTTP 403）。</p>
{{- range .Actions}}
<span class="dim">{{.Title}}（禁用）</span>
{{- end}}
{{- end}}
</section>
{{- end}}`

// indexTemplate 是预编译的内置首页主体模板。
var indexTemplate = template.Must(template.New("index").Parse(indexHTML))

// handleIndex 渲染内置首页：概览 + 页面入口 + 写操作。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	pages := s.Pages()
	data := indexPageData{
		Mode:     s.opts.Mode,
		Version:  s.opts.Version,
		Uptime:   formatUptime(time.Since(s.startedAt)),
		Writable: s.writable,
		Pages:    make([]indexPageItem, 0, len(pages)),
	}
	for _, p := range pages {
		data.Pages = append(data.Pages, indexPageItem{Title: p.Title, Path: p.Path})
	}

	if len(s.opts.Actions) > 0 {
		data.Actions = make([]indexActionItem, 0, len(s.opts.Actions))
		for _, a := range s.opts.Actions {
			method := a.Method
			if method == "" {
				method = http.MethodPost
			}
			item := indexActionItem{Title: a.Title, Name: a.Name, Method: method}
			if s.writable {
				item.Token = s.CSRFToken()
			}
			data.Actions = append(data.Actions, item)
		}
		// 只有可写模式才渲染 <form>；只读模式只列出禁用项。
		data.showForms = s.writable
	}

	var body bytes.Buffer
	if err := indexTemplate.Execute(&body, data); err != nil {
		s.renderFailed(w, r, err)
		return
	}
	s.WritePage(w, r, data, template.HTML(body.String()))
}
