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
		// 可写页一定渲染服务/主服务表单；含表单时布局默认暂停 1s 自动刷新，
		// 避免用户正在输入的内容被整页重载清空（?refresh=1 可显式开启）。
		ShowForms: func(*http.Request) bool { return a.adminWritable() },
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

// csrfToken 返回一个一次性 CSRF 令牌（未启动管理页时为空串）。
//
// 令牌是普通 string，由模板写进隐藏域时自动转义；页面数据里不使用 template.HTML。
func (a *agent) csrfToken() string {
	if a.adminSrv == nil {
		return ""
	}
	return a.adminSrv.CSRFToken()
}

// agentPageData 是 agent 管理页主体（<section> 片段）的渲染数据。
//
// 外部可控值（服务ID、本地目标、relay 下发的 agent_id/udp_mode、配对键、远端
// 地址、载体名、notice/错误文案等）一律是普通 string，由 html/template 转义。
type agentPageData struct {
	// Writable 为 true 时渲染服务/主服务写表单。
	Writable bool
	// MainToken / NewToken 是主服务表单与新增服务表单的一次性 CSRF 令牌。
	MainToken string
	NewToken  string
	// Services 是本机服务表（含每行的修改/删除表单）。
	Services []agentServiceRow
	// Main 是主服务（default）的当前值。
	Main agentMainServiceView
	// Control 是控制连接键值表。
	Control agentControlView
	// Carrier 是 UDP over TCP 承载连接状态。
	Carrier agentCarrierView
	// Sessions 是当前活跃数据连接。
	Sessions []agentSessionRow
}

// agentServiceForms 是一行服务上的写表单数据；Show=false 时只显示“只读”。
type agentServiceForms struct {
	Show        bool
	SaveToken   string
	DeleteToken string
}

// agentServiceRow 是“本机已注册服务”表格的一行。
type agentServiceRow struct {
	ID         string
	Proto      string
	Addr       string
	UDPMode    string
	Registered bool
	Forms      agentServiceForms
}

// agentMainServiceView 是主服务（default）的当前协议与本地目标。
type agentMainServiceView struct {
	Proto  string
	Local  string
	Exists bool
}

// agentKVRow 是控制连接键值表的一行；Mono=true 时值用等宽字体。
type agentKVRow struct {
	Key   string
	Value string
	Mono  bool
}

// agentControlView 是“控制连接（到 relay）”的键值数据。
type agentControlView struct {
	Rows []agentKVRow
}

// agentCarrierView 是 UDP over TCP 承载连接状态。
type agentCarrierView struct {
	// Created 为 false 时显示“未创建（懒创建）”。
	Created  bool
	Addr     string
	State    string
	Sessions int
}

// agentSessionRow 是“当前活跃数据连接”表格的一行。
type agentSessionRow struct {
	Key       string
	Proto     string
	Carrier   string
	Remote    string
	Upstream  string
	Ready     bool
	StartedAt string
	Age       string
	BytesIn   int64
	BytesOut  int64
}

// agentAdminMarkup 是 agent 管理页主体模板；只输出 <section>，布局由框架提供。
//
// 服务/协议/连接状态徽标只由模板的 {{if}} + 包内常量生成，数据里只有 bool。
const agentAdminMarkup = `<section><h2>本机已注册服务</h2>
{{- if not .Services}}
<p class="empty">尚未注册任何服务</p>
{{- else}}
<table><thead><tr><th>服务ID</th><th>协议</th><th>本地目标</th><th>udp_mode</th><th>状态</th><th>操作</th></tr></thead><tbody>
{{- range .Services}}
<tr><td class="mono">{{.ID}}</td><td>{{.Proto}}</td><td class="mono">{{.Addr}}</td><td>{{.UDPMode}}</td><td>{{if .Registered}}<span class="tag">已注册</span>{{else}}<span class="dim">未注册</span>{{end}}</td><td>{{if .Forms.Show}}<form class="inline" method="post" action="/action/agent-service-save"><input type="hidden" name="csrf_token" value="{{.Forms.SaveToken}}"><input type="hidden" name="service_id" value="{{.ID}}"><select name="proto"><option value="tcp"{{if eq .Proto "tcp"}} selected{{end}}>tcp</option><option value="udp"{{if eq .Proto "udp"}} selected{{end}}>udp</option></select> <input type="text" name="local" value="{{.Addr}}" size="18"><button type="submit">修改</button></form> <form class="inline" method="post" action="/action/agent-service-delete"><input type="hidden" name="csrf_token" value="{{.Forms.DeleteToken}}"><input type="hidden" name="service_id" value="{{.ID}}"><button type="submit" class="danger">删除</button></form>{{else}}<span class="dim">只读</span>{{end}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
<section><h2>服务配置（可写回 ini）</h2>
{{- if not .Writable}}
<p class="dim">只读模式下配置区不可编辑；请将 manage_listen 绑定到 127.0.0.1 后重启。</p>
{{- end}}
<h3 style="font-size:14px;margin:12px 0 6px">主服务（default）</h3>
{{- if not .Writable}}
<p class="mono">protocol = {{.Main.Proto}}，local = {{.Main.Local}}</p>
{{- else}}
<form method="post" action="/action/agent-main-save" class="form-inline"><input type="hidden" name="csrf_token" value="{{.MainToken}}"><label>protocol <select name="protocol"><option value="tcp"{{if eq .Main.Proto "tcp"}} selected{{end}}>tcp</option><option value="udp"{{if eq .Main.Proto "udp"}} selected{{end}}>udp</option></select></label> <label>local <input type="text" name="local" value="{{.Main.Local}}" size="28" placeholder="host:port"></label> <button type="submit">保存主服务</button></form>
{{- if not .Main.Exists}}
<p class="dim">当前没有主服务；填写后保存即可新增。</p>
{{- end}}
{{- end}}
<h3 style="font-size:14px;margin:16px 0 6px">新增服务</h3>
{{- if not .Writable}}
<p class="dim">只读模式下不可新增服务。</p>
{{- else}}
<form method="post" action="/action/agent-service-save" class="form-inline"><input type="hidden" name="csrf_token" value="{{.NewToken}}"><label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label> <label>proto <select name="proto"><option value="tcp">tcp</option><option value="udp">udp</option></select></label> <label>local <input type="text" name="local" size="24" placeholder="127.0.0.1:8080"></label> <button type="submit">新增/覆盖</button></form>
<p class="dim">服务ID 只允许 a-z0-9-_（1..64），且不能是 default；本地目标必须为 host:port。</p>
{{- end}}
</section>
<section><h2>控制连接（到 relay）</h2><table><tbody>
{{- range .Control.Rows}}
<tr><th style="width:160px">{{.Key}}</th><td{{if .Mono}} class="mono"{{end}}>{{.Value}}</td></tr>
{{- end}}
</tbody></table>
<table><thead><tr><th>UDP 承载连接</th><th>远端</th><th>状态</th><th>会话数</th></tr></thead><tbody>
{{- if .Carrier.Created}}
<tr><td>udp-over-tcp</td><td class="mono">{{.Carrier.Addr}}</td><td>{{.Carrier.State}}</td><td>{{.Carrier.Sessions}}</td></tr>
{{- else}}
<tr><td>udp-over-tcp</td><td class="mono">—</td><td>未创建（懒创建）</td><td>0</td></tr>
{{- end}}
</tbody></table></section>
<section><h2>当前活跃数据连接</h2>
{{- if not .Sessions}}
<p class="empty">当前没有活跃配对</p>
{{- else}}
<table><thead><tr><th>配对键</th><th>协议</th><th>载体</th><th>远端</th><th>本地目标</th><th>建立时间</th><th>已建立</th><th>接收字节</th><th>发送字节</th></tr></thead><tbody>
{{- range .Sessions}}
<tr><td class="mono">{{.Key}}</td><td>{{.Proto}}</td><td>{{.Carrier}}</td><td class="mono">{{.Remote}}</td><td class="mono">{{.Upstream}}</td><td>{{if .Ready}}{{.StartedAt}}{{else}}进行中{{end}}</td><td>{{if .Ready}}{{.Age}}{{else}}—{{end}}</td><td>{{.BytesIn}}</td><td>{{.BytesOut}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>`

// agentAdminTemplate 是预编译的 agent 管理页主体模板（解析失败在启动期 panic）。
var agentAdminTemplate = template.Must(template.New("agent-admin").Parse(agentAdminMarkup))

// renderAdmin 渲染管理页主体（不含 <html>/<head>/<body>，由框架套布局）。
func (a *agent) renderAdmin(w io.Writer, _ *http.Request) {
	a.writePageBody(w, agentAdminTemplate, a.agentPageView())
}

// writePageBody 把预编译模板渲染到内存缓冲后一次性写出。
//
// 渲染失败时不写出半截页面，只输出一句提示（详细错误进日志）。
func (a *agent) writePageBody(w io.Writer, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		a.log.Error("管理页渲染失败", "event", "admin_render_failed", "err", err)
		_, _ = io.WriteString(w, `<section><h2>页面渲染失败</h2><p class="dim">详见进程日志</p></section>`)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// agentPageView 从运行期状态装配管理页主体数据。
func (a *agent) agentPageView() agentPageData {
	writable := a.adminWritable()
	data := agentPageData{
		Writable: writable,
		Services: a.agentServiceRows(writable),
		Main:     a.agentMainServiceView(),
		Control:  a.agentControlView(),
		Carrier:  a.agentCarrierView(),
		Sessions: a.agentSessionRows(),
	}
	if writable {
		data.MainToken = a.csrfToken()
		data.NewToken = a.csrfToken()
	}
	return data
}

// agentServiceRows 装配本机服务表（含操作列）。
func (a *agent) agentServiceRows(writable bool) []agentServiceRow {
	services := a.svcStore.snapshot()
	registered := make(map[string]registeredService, 4)
	for _, reg := range a.registeredServices() {
		registered[reg.Service.ID] = reg
	}

	rows := make([]agentServiceRow, 0, len(services))
	for _, svc := range services {
		reg, isRegistered := registered[svc.ID]
		row := agentServiceRow{
			ID:         svc.ID,
			Proto:      svc.Proto,
			Addr:       svc.Addr,
			UDPMode:    "-",
			Registered: isRegistered,
		}
		if isRegistered && reg.UDPMode != "" {
			row.UDPMode = reg.UDPMode
		}
		// 主服务（default）在表格里只读，只能在“主服务”表单修改。
		if writable && !isDefaultService(svc.ID) {
			row.Forms = agentServiceForms{
				Show:        true,
				SaveToken:   a.csrfToken(),
				DeleteToken: a.csrfToken(),
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// agentMainServiceView 装配主服务（default）的协议与本地目标。
func (a *agent) agentMainServiceView() agentMainServiceView {
	mainSvc, hasMain := a.svcStore.get(config.DefaultServiceID)
	proto := mainSvc.Proto
	if proto == "" {
		proto = "tcp"
	}
	return agentMainServiceView{Proto: proto, Local: mainSvc.Addr, Exists: hasMain}
}

// agentControlView 装配控制连接的键值表。
func (a *agent) agentControlView() agentControlView {
	snap := a.controlSnapshot()
	state := "未连接"
	if snap.connected {
		state = "已连接"
	}
	rows := []agentKVRow{
		{Key: "relay 地址", Value: snap.addr, Mono: true},
		{Key: "状态", Value: state},
		{Key: "agent_id", Value: snap.agentID, Mono: true},
		{Key: "协议版本", Value: fmt.Sprintf("%d", snap.version)},
		{Key: "udp_mode", Value: snap.udpMode, Mono: true},
		{Key: "TCP 数据面端口", Value: fmt.Sprintf("%d", snap.tcpDataPort)},
		{Key: "UDP 数据面端口", Value: fmt.Sprintf("%d", snap.udpDataPort)},
		{Key: "心跳间隔", Value: snap.heartbeat.String()},
	}
	if !snap.since.IsZero() {
		rows = append(rows, agentKVRow{Key: "连接时长", Value: formatDuration(time.Since(snap.since))})
	}
	if !snap.lastAck.IsZero() {
		rows = append(rows, agentKVRow{
			Key:   "最近心跳应答",
			Value: snap.lastAck.Format("2006-01-02 15:04:05"),
			Mono:  true,
		})
	}
	return agentControlView{Rows: rows}
}

// agentCarrierView 装配 UDP over TCP 承载连接状态（仅 tcp_tunnel 模式有意义）。
func (a *agent) agentCarrierView() agentCarrierView {
	c := a.carrier.current()
	if c == nil {
		return agentCarrierView{}
	}
	state := "就绪"
	if c.isClosed() {
		state = "已断开"
	} else if !c.isReady() {
		state = "等待 BindOK"
	}
	return agentCarrierView{
		Created:  true,
		Addr:     c.addr,
		State:    state,
		Sessions: c.sessionCount(),
	}
}

// agentSessionRows 装配当前活跃数据连接表。
func (a *agent) agentSessionRows() []agentSessionRow {
	snaps := a.sessions.snapshot()
	rows := make([]agentSessionRow, 0, len(snaps))
	for _, s := range snaps {
		row := agentSessionRow{
			Key:      s.Key,
			Proto:    s.Proto,
			Carrier:  s.Carrier,
			Remote:   s.Remote,
			Upstream: s.Upstream,
			Ready:    s.Ready,
			BytesIn:  s.BytesIn,
			BytesOut: s.BytesOut,
		}
		if s.Ready {
			row.StartedAt = s.EstablishedAt.Format("2006-01-02 15:04:05")
			row.Age = formatDuration(time.Since(s.EstablishedAt))
		}
		rows = append(rows, row)
	}
	return rows
}

// isDefaultService 报告服务ID是否是主服务保留ID。
func isDefaultService(id string) bool {
	return id == config.DefaultServiceID
}

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
