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

// startAdmin 在 cfg.Common.ManageListen 非空时启动只读管理页。
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

	var b bytes.Buffer
	add := b.WriteString

	// 概览
	add(`<section><h2>概览</h2><table><tbody>`)
	row(&b, "relay 隧道", c.cfg.Client.RelayAddr)
	row(&b, "client_id", orDash(c.clientID))
	row(&b, "udp_mode", c.udpMode)
	row(&b, "服务列表 revision", strconv.FormatUint(revision, 10))
	row(&b, "订阅服务变更", yesNo(c.cfg.Client.Subscribe))
	row(&b, "TCP 数据面", orDash(c.tcpDataAddr))
	udpPlane := "（tcp_tunnel，复用 tcp_listen）"
	if c.udpMode == "native" {
		udpPlane = orDash(c.udpDataAddr)
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
	listeners := c.listenerViews()
	if len(listeners) == 0 {
		add(`<p class="empty">没有本地监听（[client] 未配置 service.&lt;ID&gt;）</p>`)
	} else {
		add(`<table><thead><tr><th>本地监听地址</th><th>协议</th><th>远端 service_id</th>` +
			`<th>ini 配置值</th></tr></thead><tbody>`)
		for _, l := range listeners {
			fmt.Fprintf(&b,
				`<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td class="mono">%s</td></tr>`,
				template.HTMLEscapeString(l.Addr),
				template.HTMLEscapeString(l.Proto),
				template.HTMLEscapeString(l.ServiceID),
				template.HTMLEscapeString(l.Listen))
		}
		add(`</tbody></table>`)
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
