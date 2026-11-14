package admin

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

// handleIndex 渲染内置首页：概览 + 页面入口 + 写操作。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	var b bytes.Buffer
	b.WriteString(`<section><h2>概览</h2><table><tbody>`)
	writeRow(&b, "模式", template.HTMLEscapeString(s.opts.Mode))
	writeRow(&b, "版本", template.HTMLEscapeString(s.opts.Version))
	writeRow(&b, "运行时长", template.HTMLEscapeString(formatUptime(time.Since(s.startedAt))))
	writeRow(&b, "自动刷新", "开启（可通过右上角链接暂停）")
	writeRow(&b, "配置", "只读页面；修改配置请编辑 ini 并重启进程")
	b.WriteString(`</tbody></table></section>`)

	pages := s.Pages()
	b.WriteString(`<section><h2>页面</h2>`)
	if len(pages) == 0 {
		b.WriteString(`<p class="empty">暂无页面（阶段2 的 relay / agent / client 会注册各自页面）</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>页面</th><th>路径</th></tr></thead><tbody>`)
		for _, p := range pages {
			fmt.Fprintf(&b, `<tr><td><a href="%s">%s</a></td><td class="mono">%s</td></tr>`,
				template.HTMLEscapeString(p.Path),
				template.HTMLEscapeString(p.Title),
				template.HTMLEscapeString(p.Path))
		}
		b.WriteString(`</tbody></table>`)
	}
	b.WriteString(`</section>`)

	if len(s.opts.Actions) > 0 {
		b.WriteString(`<section><h2>操作</h2><p class="dim">写操作需要一次性 CSRF 令牌，页面刷新后令牌会更新。</p>`)
		for _, a := range s.opts.Actions {
			method := a.Method
			if method == "" {
				method = http.MethodPost
			}
			fmt.Fprintf(&b, `<form class="inline" method="%s" action="/action/%s">%s<button type="submit" class="danger">%s</button></form> `,
				template.HTMLEscapeString(method),
				template.HTMLEscapeString(a.Name),
				string(s.CSRFInput()),
				template.HTMLEscapeString(a.Title))
		}
		b.WriteString(`</section>`)
	}

	s.WriteHTML(w, r, template.HTML(b.String()))
}

// writeRow 写一行键值表格。
func writeRow(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, `<tr><th style="width:120px">%s</th><td>%s</td></tr>`,
		template.HTMLEscapeString(key), value)
}
