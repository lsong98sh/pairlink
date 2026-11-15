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
		// 可写页必含新增/修改映射表单；含表单时布局默认暂停 1s 自动刷新，
		// 避免用户正在输入的内容被整页重载清空（?refresh=1 可显式开启）。
		ShowForms: func(*http.Request) bool { return c.adminWritable() },
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

// clientOverview 是「概览」区块的数据。
//
// 布尔值只以 bool 传入，是/否由模板的 {{if}} + 包内常量生成。
type clientOverview struct {
	RelayAddr   string
	ClientID    string
	UDPMode     string
	Revision    string
	Subscribe   bool
	Reconnect   bool
	TCPPlane    string
	UDPPlane    string
	SessionText string
}

// clientServiceRow 是「relay 返回的可用服务列表」表格的一行。
type clientServiceRow struct {
	ServiceID string
	Proto     string
	AgentID   string
	Online    bool
	Revision  uint64
}

// clientPairRow 是「当前活跃配对」表格的一行。
type clientPairRow struct {
	Key     string
	Proto   string
	Remote  string
	Started string
	Status  string
	ToRelay int64
	ToLocal int64
}

// clientPageData 是 client 管理页主体（<section> 片段）的渲染数据。
//
// 外部可控值（service_id、监听地址、agent_id、配对键、远端地址、notice/错误
// 文案等）一律是普通 string，由 html/template 在输出时转义。
type clientPageData struct {
	// Writable 为 true 时渲染新增/修改/删除映射表单。
	Writable bool
	Overview clientOverview
	Services []clientServiceRow
	Mappings []clientMappingRow
	// NewToken 是「新增映射」表单的一次性 CSRF 令牌。
	NewToken string
	Pairs    []clientPairRow
}

// clientAdminMarkup 是 client 管理页主体模板；只输出 <section>，布局由框架提供。
const clientAdminMarkup = `<section><h2>概览</h2><table><tbody>
<tr><th style="width:220px">relay 隧道</th><td class="mono">{{.Overview.RelayAddr}}</td></tr>
<tr><th style="width:220px">client_id</th><td class="mono">{{.Overview.ClientID}}</td></tr>
<tr><th style="width:220px">udp_mode</th><td class="mono">{{.Overview.UDPMode}}</td></tr>
<tr><th style="width:220px">服务列表 revision</th><td class="mono">{{.Overview.Revision}}</td></tr>
<tr><th style="width:220px">订阅服务变更</th><td class="mono">{{if .Overview.Subscribe}}是{{else}}否{{end}}</td></tr>
<tr><th style="width:220px">自动重连</th><td class="mono">{{if .Overview.Reconnect}}是{{else}}否{{end}}</td></tr>
<tr><th style="width:220px">TCP 数据面</th><td class="mono">{{.Overview.TCPPlane}}</td></tr>
<tr><th style="width:220px">UDP 数据面</th><td class="mono">{{.Overview.UDPPlane}}</td></tr>
<tr><th style="width:220px">本地 TCP 连接 / UDP 会话</th><td class="mono">{{.Overview.SessionText}}</td></tr>
</tbody></table></section>
<section><h2>relay 返回的可用服务列表</h2>
{{- if not .Services}}
<p class="empty">服务列表为空（relay 上还没有 agent 注册服务）</p>
{{- else}}
<table><thead><tr><th>service_id</th><th>协议</th><th>agent_id</th><th>在线</th><th>revision</th></tr></thead><tbody>
{{- range .Services}}
<tr><td class="mono">{{.ServiceID}}</td><td>{{.Proto}}</td><td class="mono">{{.AgentID}}</td><td>{{if .Online}}是{{else}}否{{end}}</td><td class="mono">{{.Revision}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
<section><h2>本地端口映射表</h2>
{{- if not .Writable}}
<p class="dim">只读模式下映射不可编辑；请将 manage_listen 绑定到 127.0.0.1 后重启。</p>
{{- end}}
{{- if not .Mappings}}
<p class="empty">没有本地监听（[client] 未配置 service.&lt;ID&gt;）</p>
{{- else}}
<table><thead><tr><th>service_id</th><th>本地监听地址</th><th>实际绑定</th><th>远端 proto</th><th>操作</th></tr></thead><tbody>
{{- range .Mappings}}
<tr><td class="mono">{{.ServiceID}}</td><td class="mono">{{.Listen}}</td><td class="mono">{{.Actual}}</td><td>{{.Proto}}</td><td>{{if .Forms.Show}}<form class="inline" method="post" action="/action/client-mapping-save"><input type="hidden" name="csrf_token" value="{{.Forms.SaveToken}}"><input type="hidden" name="service_id" value="{{.ServiceID}}"><input type="text" name="listen" value="{{.Listen}}" size="18"><button type="submit">修改</button></form> <form class="inline" method="post" action="/action/client-mapping-delete"><input type="hidden" name="csrf_token" value="{{.Forms.DeleteToken}}"><input type="hidden" name="service_id" value="{{.ServiceID}}"><button type="submit" class="danger">删除</button></form>{{else}}<span class="dim">只读</span>{{end}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
{{- if .Writable}}
<h3 style="font-size:14px;margin:14px 0 6px">新增映射</h3><form method="post" action="/action/client-mapping-save" class="form-inline"><input type="hidden" name="csrf_token" value="{{.NewToken}}"><label>service_id <input type="text" name="service_id" size="14" placeholder="web"></label> <label>本地监听 <input type="text" name="listen" size="20" placeholder="127.0.0.1:8080"></label> <button type="submit">新增映射</button></form><p class="dim">service_id 只允许 a-z0-9-_（1..64），且不能是 default；本地监听地址允许裸端口（补 127.0.0.1）。</p>
{{- end}}
</section>
<section><h2>当前活跃配对</h2>
{{- if not .Pairs}}
<p class="empty">当前没有活跃配对</p>
{{- else}}
<table><thead><tr><th>配对键</th><th>协议</th><th>来源</th><th>开始时间</th><th>状态</th><th>本地→relay 字节</th><th>relay→本地 字节</th></tr></thead><tbody>
{{- range .Pairs}}
<tr><td class="mono">{{.Key}}</td><td>{{.Proto}}</td><td class="mono">{{.Remote}}</td><td class="mono">{{.Started}}</td><td>{{.Status}}</td><td class="mono">{{.ToRelay}}</td><td class="mono">{{.ToLocal}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>`

// clientAdminTemplate 是预编译的 client 管理页主体模板（解析失败在启动期 panic）。
var clientAdminTemplate = template.Must(template.New("client-admin").Parse(clientAdminMarkup))

// renderAdminPage 渲染 client 管理页主体：
// 概览、relay 服务列表、本地端口映射表、当前活跃配对。
func (c *client) renderAdminPage(w io.Writer, _ *http.Request) {
	c.writePageBody(w, clientAdminTemplate, c.clientPageView())
}

// writePageBody 把预编译模板渲染到内存缓冲后一次性写出。
//
// 渲染失败时不写出半截页面，只输出一句提示（详细错误进日志）。
func (c *client) writePageBody(w io.Writer, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		c.log.Error("管理页渲染失败", "event", "admin_render_failed", "err", err)
		_, _ = io.WriteString(w, `<section><h2>页面渲染失败</h2><p class="dim">详见进程日志</p></section>`)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// clientPageView 从运行期状态装配管理页主体数据。
func (c *client) clientPageView() clientPageData {
	writable := c.adminWritable()
	data := clientPageData{
		Writable: writable,
		Overview: c.clientOverviewView(),
		Services: c.clientServiceRows(),
		Mappings: c.clientMappingRows(),
		Pairs:    c.clientPairRows(),
	}
	if writable {
		data.NewToken = c.csrfToken()
	}
	return data
}

// clientOverviewView 装配「概览」区块。
func (c *client) clientOverviewView() clientOverview {
	_, revision := c.servicesView()
	neg := c.negotiated()
	udpPlane := "（tcp_tunnel，复用 tcp_listen）"
	if neg.udpMode == "native" {
		udpPlane = orDash(neg.udpDataAddr)
	}
	return clientOverview{
		RelayAddr:   c.cfg.Client.RelayAddr,
		ClientID:    orDash(neg.clientID),
		UDPMode:     neg.udpMode,
		Revision:    strconv.FormatUint(revision, 10),
		Subscribe:   c.cfg.Client.Subscribe,
		Reconnect:   c.cfg.Client.Reconnect,
		TCPPlane:    orDash(neg.tcpDataAddr),
		UDPPlane:    udpPlane,
		SessionText: fmt.Sprintf("%d / %d", c.sessionCount(), c.udpSessionCount()),
	}
}

// clientServiceRows 装配「relay 返回的可用服务列表」（按 service_id 排序）。
func (c *client) clientServiceRows() []clientServiceRow {
	services, revision := c.servicesView()
	ids := make([]string, 0, len(services))
	for id := range services {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := make([]clientServiceRow, 0, len(ids))
	for _, id := range ids {
		svc := services[id]
		rows = append(rows, clientServiceRow{
			ServiceID: svc.ServiceID,
			Proto:     svc.Proto,
			AgentID:   orDash(svc.AgentID),
			Online:    svc.Online,
			Revision:  revision,
		})
	}
	return rows
}

// clientPairRows 装配「当前活跃配对」表格。
func (c *client) clientPairRows() []clientPairRow {
	pairs := c.pairViews()
	rows := make([]clientPairRow, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, clientPairRow{
			Key:     p.Key,
			Proto:   p.Proto,
			Remote:  orDash(p.Remote),
			Started: p.Started.Format("2006-01-02 15:04:05"),
			Status:  p.Status,
			ToRelay: p.ToRelay,
			ToLocal: p.ToLocal,
		})
	}
	return rows
}

// adminWritable 报告管理页是否可写（未启动管理页时视为只读）。
func (c *client) adminWritable() bool {
	return c.admin != nil && c.admin.Writable()
}

// csrfToken 返回一个一次性 CSRF 令牌（未启动管理页时为空串）。
//
// 令牌是普通 string，由模板写进隐藏域时自动转义；页面数据里不使用 template.HTML。
func (c *client) csrfToken() string {
	if c.admin == nil {
		return ""
	}
	return c.admin.CSRFToken()
}

// orDash 把空串转成 "-"，避免表格出现空白单元格。
func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
