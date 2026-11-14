package client

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/version"
)

// startAdmin 在 cfg.Common.ManageListen 非空时启动管理页（阶段 7 起可写）。
func (c *client) startAdmin() error {
	if c.cfg.Common.ManageListen == "" {
		return nil
	}
	srv := admin.New(c.cfg.Common.ManageListen, admin.Options{
		Title:   "PairLink client 管理页",
		Mode:    string(config.ModeClient),
		Version: version.Version,
		Logger:  c.log,
		Pprof:   c.cfg.Common.Pprof,
		Actions: []admin.Action{
			{Name: "client-mapping-save", Title: "新增/修改映射", Method: http.MethodPost, Handler: c.handleMappingSave},
			{Name: "client-mapping-delete", Title: "删除映射", Method: http.MethodPost, Handler: c.handleMappingDelete},
		},
	})
	srv.AddPage(admin.Page{
		Path:   "/client",
		Title:  "客户端状态",
		Render: c.renderAdminPage,
	})
	// 先发布 c.admin 再 Start：与 relay 一致，避免管理页 handler 与赋值并发读写。
	c.admin = srv
	if err := srv.Start(c.ctx); err != nil {
		c.admin = nil
		return fmt.Errorf("client: 启动管理页: %w", err)
	}
	c.adminAddr = srv.Addr()
	c.log.Info("管理页已启动", "event", "admin_start", "detail", "manage_listen="+c.adminAddr)
	return nil
}

// renderAdminPage 渲染 client 管理页主体（只读）：
// 概览、relay 服务列表、本地端口映射表、当前活跃配对。
func (c *client) renderAdminPage(w io.Writer, _ *http.Request) {
	services, revision := c.servicesView()
	neg := c.negotiated()

	var b bytes.Buffer
	add := b.WriteString

	// 概览
	add(`<section><h2>概览</h2><table><tbody>`)
	row(&b, "relay 隧道", c.cfg.Client.RelayAddr)
	row(&b, "client_id", orDash(neg.clientID))
	row(&b, "udp_mode", neg.udpMode)
	row(&b, "服务列表 revision", strconv.FormatUint(revision, 10))
	row(&b, "订阅服务变更", yesNo(c.cfg.Client.Subscribe))
	row(&b, "自动重连", yesNo(c.cfg.Client.Reconnect))
	row(&b, "TCP 数据面", orDash(neg.tcpDataAddr))
	udpPlane := "（tcp_tunnel，复用 tcp_listen）"
	if neg.udpMode == "native" {
		udpPlane = orDash(neg.udpDataAddr)
	}
	row(&b, "UDP 数据面", udpPlane)
	row(&b, "本地 TCP 连接 / UDP 会话", fmt.Sprintf("%d / %d", c.sessionCount(), c.udpSessionCount()))
	add(`</tbody></table></section>`)

	// relay 服务列表
	add(`<section><h2>relay 返回的可用服务列表</h2>`)
	if len(services) == 0 {
		add(`<p class="empty">服务列表为空（relay 上还没有 agent 注册服务）</p>`)
	} else {
		add(`<table><thead><tr><th>service_id</th><th>协议</th><th>agent_id</th><th>在线</th>` +
			`<th>revision</th></tr></thead><tbody>`)
		ids := make([]string, 0, len(services))
		for id := range services {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			svc := services[id]
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td>%s</td><td class="mono">%d</td></tr>`,
				template.HTMLEscapeString(svc.ServiceID),
				template.HTMLEscapeString(svc.Proto),
				template.HTMLEscapeString(orDash(svc.AgentID)),
				yesNo(svc.Online),
				revision)
		}
		add(`</tbody></table>`)
	}
	add(`</section>`)

	// 本地端口映射表
	add(`<section><h2>本地端口映射表</h2>`)
	if !c.adminWritable() {
		add(`<p class="dim">只读模式下映射不可编辑；请将 manage_listen 绑定到 127.0.0.1 后重启。</p>`)
	}
	mappings := c.mappingViews()
	if len(mappings) == 0 {
		add(`<p class="empty">没有本地监听（[client] 未配置 service.&lt;ID&gt;）</p>`)
	} else {
		add(`<table><thead><tr><th>service_id</th><th>本地监听地址</th><th>实际绑定</th>` +
			`<th>远端 proto</th><th>操作</th></tr></thead><tbody>`)
		for _, m := range mappings {
			actions := `<span class="dim">只读</span>`
			if c.adminWritable() && m.ServiceID != config.DefaultServiceID {
				actions = fmt.Sprintf(`<form class="inline" method="post" action="/action/client-mapping-save">%s`+
					`<input type="hidden" name="service_id" value="%s"><input type="text" name="listen" value="%s" size="18">`+
					`<button type="submit">修改</button></form> `+
					`<form class="inline" method="post" action="/action/client-mapping-delete">%s`+
					`<input type="hidden" name="service_id" value="%s"><button type="submit" class="danger">删除</button></form>`,
					string(c.csrfInput()), template.HTMLEscapeString(m.ServiceID), template.HTMLEscapeString(m.Listen),
					string(c.csrfInput()), template.HTMLEscapeString(m.ServiceID))
			}
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td></tr>`,
				template.HTMLEscapeString(m.ServiceID),
				template.HTMLEscapeString(m.Listen),
				template.HTMLEscapeString(orDash(m.Actual)),
				template.HTMLEscapeString(m.Proto),
				actions)
		}
		add(`</tbody></table>`)
	}
	if c.adminWritable() {
		fmt.Fprintf(&b, `<h3 style="font-size:14px;margin:14px 0 6px">新增映射</h3>`+
			`<form method="post" action="/action/client-mapping-save" class="form-inline">%s`+
			`<label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label> `+
			`<label>本地监听 <input type="text" name="listen" size="20" placeholder="127.0.0.1:8080"></label> `+
			`<button type="submit">新增映射</button></form>`,
			string(c.csrfInput()))
		add(`<p class="dim">service_id 只允许 a-z0-9-_（1..64），且不能是 default；本地监听地址允许裸端口（补 127.0.0.1）。</p>`)
	}
	add(`</section>`)

	// 当前活跃配对
	add(`<section><h2>当前活跃配对</h2>`)
	pairs := c.pairViews()
	if len(pairs) == 0 {
		add(`<p class="empty">当前没有活跃配对</p>`)
	} else {
		add(`<table><thead><tr><th>配对键</th><th>协议</th><th>来源</th><th>开始时间</th>` +
			`<th>状态</th><th>本地→relay 字节</th><th>relay→本地 字节</th></tr></thead><tbody>`)
		for _, p := range pairs {
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td class="mono">%s</td>`+
					`<td>%s</td><td class="mono">%d</td><td class="mono">%d</td></tr>`,
				template.HTMLEscapeString(p.Key),
				template.HTMLEscapeString(p.Proto),
				template.HTMLEscapeString(orDash(p.Remote)),
				template.HTMLEscapeString(p.Started.Format("2006-01-02 15:04:05")),
				template.HTMLEscapeString(p.Status),
				p.ToRelay, p.ToLocal)
		}
		add(`</tbody></table>`)
	}
	add(`</section>`)

	_, _ = w.Write(b.Bytes())
}

// adminWritable 报告管理页是否可写（未启动管理页时视为只读）。
func (c *client) adminWritable() bool {
	return c.admin != nil && c.admin.Writable()
}

// csrfInput 返回表单 CSRF 隐藏域（未启动管理页时为空）。
func (c *client) csrfInput() template.HTML {
	if c.admin == nil {
		return ""
	}
	return c.admin.CSRFInput()
}

// row 写一行键值表格（值做 HTML 转义）。
func row(b *bytes.Buffer, key, value string) {
	b.WriteString(`<tr><th style="width:220px">`)
	b.WriteString(template.HTMLEscapeString(key))
	b.WriteString(`</th><td class="mono">`)
	b.WriteString(template.HTMLEscapeString(value))
	b.WriteString(`</td></tr>`)
}

// yesNo 把布尔值转成中文。
func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

// orDash 把空串转成 "-"，避免表格出现空白单元格。
func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
