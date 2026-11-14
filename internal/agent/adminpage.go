package agent

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/version"
)

// startAdmin 在 [common] manage_listen 非空时启动管理页。
//
// 页面展示 spec §5 / docs/contract.md §3.8 要求的三块内容：
//   - 本机服务表（可新增/修改/删除）与主服务编辑；
//   - 到 relay 的控制连接状态（地址、连接时长、udp_mode、心跳）；
//   - 当前活跃数据连接（配对键、协议、载体、远端、建立时间、收发字节）。
//
// 阶段 7 起写能力仅在 manage_listen 绑定回环时启用（由 internal/admin 判定）。
func (a *agent) startAdmin() error {
	addr := a.cfg.Common.ManageListen
	if addr == "" {
		return nil
	}
	srv := admin.New(addr, admin.Options{
		Title:   "PairLink agent 管理页",
		Mode:    modeName,
		Version: version.Version,
		Logger:  a.log,
		Pprof:   a.cfg.Common.Pprof,
		Actions: []admin.Action{
			{Name: "agent-service-save", Title: "新增/修改服务", Method: http.MethodPost, Handler: a.handleServiceSave},
			{Name: "agent-service-delete", Title: "删除服务", Method: http.MethodPost, Handler: a.handleServiceDelete},
			{Name: "agent-main-save", Title: "修改主服务", Method: http.MethodPost, Handler: a.handleMainSave},
		},
	})
	srv.AddPage(admin.Page{
		Path:   "/status",
		Title:  "运行状态",
		Render: a.renderAdmin,
	})
	// 先发布 a.adminSrv 再 Start：与 relay 一致，避免管理页 handler 与赋值并发读写。
	a.adminSrv = srv
	if err := srv.Start(a.ctx); err != nil {
		a.adminSrv = nil
		return fmt.Errorf("agent: 启动管理页 %s: %w", addr, err)
	}
	return nil
}

// adminWritable 报告管理页是否可写（未启动管理页时视为只读）。
func (a *agent) adminWritable() bool {
	return a.adminSrv != nil && a.adminSrv.Writable()
}

// csrfInput 返回表单 CSRF 隐藏域（未启动管理页时为空）。
func (a *agent) csrfInput() template.HTML {
	if a.adminSrv == nil {
		return ""
	}
	return a.adminSrv.CSRFInput()
}

// renderAdmin 渲染管理页主体（不含 <html>/<head>/<body>，由框架套布局）。
func (a *agent) renderAdmin(w io.Writer, r *http.Request) {
	var b bytes.Buffer
	a.renderServices(&b)
	a.renderConfig(&b)
	a.renderControl(&b)
	a.renderSessions(&b)
	_, _ = io.WriteString(w, b.String())
}

// renderServices 渲染本机服务表（含操作列）。
func (a *agent) renderServices(b *bytes.Buffer) {
	services := a.svcStore.snapshot()
	registered := make(map[string]registeredService, 4)
	for _, reg := range a.registeredServices() {
		registered[reg.Service.ID] = reg
	}

	b.WriteString(`<section><h2>本机已注册服务</h2>`)
	if len(services) == 0 {
		b.WriteString(`<p class="empty">尚未注册任何服务</p></section>`)
		return
	}
	b.WriteString(`<table><thead><tr><th>服务ID</th><th>协议</th><th>本地目标</th><th>udp_mode</th><th>状态</th><th>操作</th></tr></thead><tbody>`)
	for _, svc := range services {
		reg, isRegistered := registered[svc.ID]
		mode := reg.UDPMode
		status := `<span class="dim">未注册</span>`
		if isRegistered && reg.UDPMode != "" {
			status = `<span class="tag">已注册</span>`
		} else if isRegistered {
			status = `<span class="tag">已注册</span>`
			mode = "-"
		} else {
			mode = "-"
		}
		actions := `<span class="dim">只读</span>`
		if a.adminWritable() && !isDefaultService(svc.ID) {
			actions = fmt.Sprintf(`<form class="inline" method="post" action="/action/agent-service-save">%s`+
				`<input type="hidden" name="service_id" value="%s">`+
				`<select name="proto"><option value="tcp"%s>tcp</option><option value="udp"%s>udp</option></select> `+
				`<input type="text" name="local" value="%s" size="18"><button type="submit">修改</button></form> `+
				`<form class="inline" method="post" action="/action/agent-service-delete">%s`+
				`<input type="hidden" name="service_id" value="%s"><button type="submit" class="danger">删除</button></form>`,
				string(a.csrfInput()), esc(svc.ID),
				selected(svc.Proto == "tcp"), selected(svc.Proto == "udp"), esc(svc.Addr),
				string(a.csrfInput()), esc(svc.ID))
		}
		fmt.Fprintf(b, `<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			esc(svc.ID), esc(svc.Proto), esc(svc.Addr), esc(mode), status, actions)
	}
	b.WriteString(`</tbody></table></section>`)
}

// isDefaultService 报告服务ID是否是主服务保留ID。
func isDefaultService(id string) bool {
	return id == config.DefaultServiceID
}

// renderConfig 渲染服务配置区：主服务编辑表单 + 新增服务表单。
func (a *agent) renderConfig(b *bytes.Buffer) {
	writable := a.adminWritable()
	b.WriteString(`<section><h2>服务配置（可写回 ini）</h2>`)
	if !writable {
		b.WriteString(`<p class="dim">只读模式下配置区不可编辑；请将 manage_listen 绑定到 127.0.0.1 后重启。</p>`)
	}

	mainSvc, hasMain := a.svcStore.get(config.DefaultServiceID)
	proto := mainSvc.Proto
	if proto == "" {
		proto = "tcp"
	}
	local := mainSvc.Addr
	b.WriteString(`<h3 style="font-size:14px;margin:12px 0 6px">主服务（default）</h3>`)
	if !writable {
		fmt.Fprintf(b, `<p class="mono">protocol = %s，local = %s</p>`, esc(proto), esc(local))
	} else {
		fmt.Fprintf(b, `<form method="post" action="/action/agent-main-save" class="form-inline">%s`+
			`<label>protocol <select name="protocol"><option value="tcp"%s>tcp</option><option value="udp"%s>udp</option></select></label> `+
			`<label>local <input type="text" name="local" value="%s" size="28" placeholder="host:port"></label> `+
			`<button type="submit">保存主服务</button></form>`,
			string(a.csrfInput()),
			selected(proto == "tcp"), selected(proto == "udp"), esc(local))
		if !hasMain {
			b.WriteString(`<p class="dim">当前没有主服务；填写后保存即可新增。</p>`)
		}
	}

	b.WriteString(`<h3 style="font-size:14px;margin:16px 0 6px">新增服务</h3>`)
	if !writable {
		b.WriteString(`<p class="dim">只读模式下不可新增服务。</p>`)
	} else {
		fmt.Fprintf(b, `<form method="post" action="/action/agent-service-save" class="form-inline">%s`+
			`<label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label> `+
			`<label>proto <select name="proto"><option value="tcp">tcp</option><option value="udp">udp</option></select></label> `+
			`<label>local <input type="text" name="local" size="24" placeholder="127.0.0.1:8080"></label> `+
			`<button type="submit">新增/覆盖</button></form>`,
			string(a.csrfInput()))
		b.WriteString(`<p class="dim">服务ID 只允许 a-z0-9-_（1..64），且不能是 default；本地目标必须为 host:port。</p>`)
	}
	b.WriteString(`</section>`)
}

// selected 返回 select option 的 selected 属性。
func selected(ok bool) string {
	if ok {
		return " selected"
	}
	return ""
}

// renderControl 渲染控制连接状态。
func (a *agent) renderControl(b *bytes.Buffer) {
	snap := a.controlSnapshot()
	b.WriteString(`<section><h2>控制连接（到 relay）</h2><table><tbody>`)
	writeKV(b, "relay 地址", snap.addr, true)
	state := "未连接"
	if snap.connected {
		state = "已连接"
	}
	writeKV(b, "状态", state, false)
	writeKV(b, "agent_id", snap.agentID, true)
	writeKV(b, "协议版本", fmt.Sprintf("%d", snap.version), false)
	writeKV(b, "udp_mode", snap.udpMode, true)
	writeKV(b, "TCP 数据面端口", fmt.Sprintf("%d", snap.tcpDataPort), false)
	writeKV(b, "UDP 数据面端口", fmt.Sprintf("%d", snap.udpDataPort), false)
	writeKV(b, "心跳间隔", snap.heartbeat.String(), false)
	if !snap.since.IsZero() {
		writeKV(b, "连接时长", formatDuration(time.Since(snap.since)), false)
	}
	if !snap.lastAck.IsZero() {
		writeKV(b, "最近心跳应答", snap.lastAck.Format("2006-01-02 15:04:05"), true)
	}
	b.WriteString(`</tbody></table>`)

	// UDP over TCP 承载连接状态（仅 tcp_tunnel 模式有意义）。
	b.WriteString(`<table><thead><tr><th>UDP 承载连接</th><th>远端</th><th>状态</th><th>会话数</th></tr></thead><tbody>`)
	c := a.carrier.current()
	if c == nil {
		b.WriteString(`<tr><td>udp-over-tcp</td><td class="mono">—</td><td>未创建（懒创建）</td><td>0</td></tr>`)
	} else {
		state := "就绪"
		if c.isClosed() {
			state = "已断开"
		} else if !c.isReady() {
			state = "等待 BindOK"
		}
		fmt.Fprintf(b, `<tr><td>udp-over-tcp</td><td class="mono">%s</td><td>%s</td><td>%d</td></tr>`,
			esc(c.addr), esc(state), c.sessionCount())
	}
	b.WriteString(`</tbody></table></section>`)
}

// renderSessions 渲染当前活跃数据连接。
func (a *agent) renderSessions(b *bytes.Buffer) {
	snaps := a.sessions.snapshot()
	b.WriteString(`<section><h2>当前活跃数据连接</h2>`)
	if len(snaps) == 0 {
		b.WriteString(`<p class="empty">当前没有活跃配对</p></section>`)
		return
	}
	b.WriteString(`<table><thead><tr><th>配对键</th><th>协议</th><th>载体</th><th>远端</th><th>本地目标</th><th>建立时间</th><th>已建立</th><th>接收字节</th><th>发送字节</th></tr></thead><tbody>`)
	for _, s := range snaps {
		established := "进行中"
		age := "—"
		if s.Ready {
			established = s.EstablishedAt.Format("2006-01-02 15:04:05")
			age = formatDuration(time.Since(s.EstablishedAt))
		}
		fmt.Fprintf(b, `<tr><td class="mono">%s</td><td>%s</td><td>%s</td><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%d</td><td>%d</td></tr>`,
			esc(s.Key), esc(s.Proto), esc(s.Carrier), esc(s.Remote), esc(s.Upstream),
			esc(established), esc(age), s.BytesIn, s.BytesOut)
	}
	b.WriteString(`</tbody></table></section>`)
}

// writeKV 写一行键值表格。
func writeKV(b *bytes.Buffer, key, value string, mono bool) {
	class := ""
	if mono {
		class = ` class="mono"`
	}
	fmt.Fprintf(b, `<tr><th style="width:160px">%s</th><td%s>%s</td></tr>`,
		esc(key), class, esc(value))
}

// esc 转义动态文本。
func esc(s string) string { return template.HTMLEscapeString(s) }

// formatDuration 把时长格式化为秒级可读字符串。
func formatDuration(d time.Duration) string {
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
