package relay

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// relayPageData 是 relay 管理页主体（<section> 片段）的渲染数据。
//
// 所有字段都由包内装配函数从运行期状态复制而来：外部可控值（服务ID、端点、
// agent ID、配对键、配置值等）一律是普通 string，由 html/template 在输出时
// 自动转义；数据里不携带任何 template.HTML。
type relayPageData struct {
	Overview []relayOverviewRow
	Agents   []relayAgentRow
	Services []relayServiceRow
	Pairs    []relayPairRow
	UDP      relayUDPSection
	Config   relayConfigData
}

// relayOverviewRow 是“概览”表格的一行。
type relayOverviewRow struct {
	Key   string
	Value string
}

// relayKickView 是一个“踢出”表单的渲染数据。
//
// Show 为 false（非回环只读模式）时模板只输出“只读”占位，不渲染表单，也不
// 下发 CSRF 令牌。
type relayKickView struct {
	Show  bool
	Kind  string
	ID    string
	Token string
}

// relayAgentRow 是“在线 agent”表格的一行。
type relayAgentRow struct {
	ID          string
	RemoteAddr  string
	ConnectedAt string
	LastSeen    string
	Services    string
	Kick        relayKickView
}

// relayServiceRow 是“服务注册表”表格的一行。
type relayServiceRow struct {
	ID           string
	Proto        string
	Online       bool
	AgentID      string
	AgentAddr    string
	RegisteredAt string
	Kick         relayKickView
}

// relayPairRow 是“当前配对表”表格的一行。
type relayPairRow struct {
	Key           string
	AgentArrived  bool
	ClientArrived bool
	Completed     bool
	Remaining     string
	Kick          relayKickView
}

// relayUDPSection 是“UDP 会话表”区块的数据（native 与 UDP over TCP 两种形态）。
type relayUDPSection struct {
	// Native 为 true 时表的列是 Agent/Client 端点；否则是承载连接。
	Native bool
	Rows   []relayUDPSessionRow
	// Empty 是空表的提示文案（两种形态不同）。
	Empty string
}

// relayUDPSessionRow 是 UDP 会话表的一行。
type relayUDPSessionRow struct {
	Key        string
	AgentSide  string
	ClientSide string
	Complete   bool
	CreatedAt  string
	LastSeen   string
	// Kick 仅 UDP over TCP 形态有操作列。
	Kick relayKickView
}

// relayKickMarkup 是“踢出”表单的模板片段（define），四个表格共用。
//
// 隐藏域的相邻顺序（csrf_token → kind → id）与旧输出一致：既有 e2e 用正则
// 直接匹配这三段连续字节。
const relayKickMarkup = `{{define "kick"}}{{if .Show}}<form class="inline" method="post" action="/action/kick"><input type="hidden" name="csrf_token" value="{{.Token}}"><input type="hidden" name="kind" value="{{.Kind}}"><input type="hidden" name="id" value="{{.ID}}"><button type="submit" class="danger">踢出</button></form>{{else}}<span class="dim">只读</span>{{end}}{{end}}`

// relayAdminMarkup 是 relay 管理页主体模板；只输出 <section>，布局由框架提供。
//
// 「在线/离线」「完整/半开」等状态徽标只由模板的 {{if}} + 包内常量生成，数据里
// 只有 bool，避免把外部字符串当标记拼进 HTML。
const relayAdminMarkup = `<section><h2>概览</h2><table><tbody>
{{- range .Overview}}
<tr><th style="width:180px">{{.Key}}</th><td>{{.Value}}</td></tr>
{{- end}}
</tbody></table></section>
<section><h2>在线 agent（{{len .Agents}}）</h2>
{{- if not .Agents}}
<p class="empty">当前没有在线 agent</p>
{{- else}}
<table><thead><tr><th>Agent ID</th><th>对端地址</th><th>连接时间</th><th>最后活跃</th><th>服务</th><th>操作</th></tr></thead><tbody>
{{- range .Agents}}
<tr><td class="mono">{{.ID}}</td><td class="mono">{{.RemoteAddr}}</td><td>{{.ConnectedAt}}</td><td>{{.LastSeen}}</td><td>{{.Services}}</td><td>{{template "kick" .Kick}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
<section><h2>服务注册表（{{len .Services}}）</h2>
{{- if not .Services}}
<p class="empty">还没有 agent 注册服务</p>
{{- else}}
<table><thead><tr><th>服务ID</th><th>协议</th><th>在线状态</th><th>所属 agent</th><th>Agent 地址</th><th>注册时间</th><th>操作</th></tr></thead><tbody>
{{- range .Services}}
<tr><td class="mono">{{.ID}}</td><td>{{.Proto}}</td><td>{{if .Online}}<span class="tag">在线</span>{{else}}<span class="dim">离线</span>{{end}}</td><td class="mono">{{.AgentID}}</td><td class="mono">{{.AgentAddr}}</td><td>{{.RegisteredAt}}</td><td>{{template "kick" .Kick}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
<section><h2>当前配对表（{{len .Pairs}}）</h2>
{{- if not .Pairs}}
<p class="empty">当前没有配对占位</p>
{{- else}}
<table><thead><tr><th>配对键</th><th>Agent 已到</th><th>Client 已到</th><th>已完成</th><th>剩余</th><th>操作</th></tr></thead><tbody>
{{- range .Pairs}}
<tr><td class="mono">{{.Key}}</td><td>{{if .AgentArrived}}是{{else}}否{{end}}</td><td>{{if .ClientArrived}}是{{else}}否{{end}}</td><td>{{if .Completed}}是{{else}}否{{end}}</td><td>{{.Remaining}}</td><td>{{template "kick" .Kick}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
<section><h2>UDP 会话表（{{if .UDP.Native}}native{{else}}UDP over TCP{{end}}，{{len .UDP.Rows}}）</h2>
{{- if not .UDP.Rows}}
<p class="empty">{{.UDP.Empty}}</p>
{{- else if .UDP.Native}}
<table><thead><tr><th>配对键</th><th>Agent 端点</th><th>Client 端点</th><th>状态</th><th>创建时间</th><th>最后活跃</th></tr></thead><tbody>
{{- range .UDP.Rows}}
<tr><td class="mono">{{.Key}}</td><td class="mono">{{.AgentSide}}</td><td class="mono">{{.ClientSide}}</td><td>{{if .Complete}}完整{{else}}半开{{end}}</td><td>{{.CreatedAt}}</td><td>{{.LastSeen}}</td></tr>
{{- end}}
</tbody></table>
{{- else}}
<table><thead><tr><th>配对键</th><th>Agent 承载</th><th>Client 承载</th><th>状态</th><th>创建时间</th><th>最后活跃</th><th>操作</th></tr></thead><tbody>
{{- range .UDP.Rows}}
<tr><td class="mono">{{.Key}}</td><td class="mono">{{.AgentSide}}</td><td class="mono">{{.ClientSide}}</td><td>{{if .Complete}}完整{{else}}半开{{end}}</td><td>{{.CreatedAt}}</td><td>{{.LastSeen}}</td><td>{{template "kick" .Kick}}</td></tr>
{{- end}}
</tbody></table>
{{- end}}
</section>
{{template "relay-config" .Config}}`

// relayAdminTemplate 是预编译的 relay 管理页主体模板（页面 + 踢出表单片段 +
// 配置区块）。解析失败会在进程启动期 panic，而不是等到请求时才发现。
var relayAdminTemplate = template.Must(
	template.New("relay-admin").Parse(relayAdminMarkup + relayKickMarkup + relayConfigMarkup))

// renderAdmin 渲染 relay 管理页主体（只写 <section>，布局由 internal/admin 提供）。
func (s *server) renderAdmin(w io.Writer, _ *http.Request) {
	s.writePageBody(w, relayAdminTemplate, s.relayPageView())
}

// writePageBody 把预编译模板渲染到内存缓冲后一次性写出。
//
// 渲染失败时不写出半截页面，只输出一句提示（详细错误进日志）。
func (s *server) writePageBody(w io.Writer, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		s.log.Error("管理页渲染失败", "event", "admin_render_failed", "err", err)
		_, _ = io.WriteString(w, `<section><h2>页面渲染失败</h2><p class="dim">详见进程日志</p></section>`)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// relayPageView 从运行期状态装配管理页主体数据。
func (s *server) relayPageView() relayPageData {
	return relayPageData{
		Overview: s.relayOverviewRows(),
		Agents:   s.relayAgentRows(),
		Services: s.relayServiceRows(),
		Pairs:    s.relayPairRows(),
		UDP:      s.relayUDPSectionView(),
		Config:   s.relayConfigView(),
	}
}

// relayOverviewRows 装配“概览”表格（键永为包内常量，值可能来自配置/运行期）。
func (s *server) relayOverviewRows() []relayOverviewRow {
	rows := make([]relayOverviewRow, 0, 7)
	rows = append(rows, relayOverviewRow{Key: "UDP 承载模式", Value: s.udpMode})
	rows = append(rows, relayOverviewRow{
		Key:   "TCP 数据面",
		Value: fmt.Sprintf("%s（端口 %d）", s.tcpAddr, s.tcpPort),
	})
	if s.udpMode == protocol.UDPModeNative {
		rows = append(rows, relayOverviewRow{
			Key:   "UDP 数据面",
			Value: fmt.Sprintf("%s（端口 %d）", s.udpAddr, s.udpPort),
		})
	} else {
		rows = append(rows, relayOverviewRow{
			Key:   "UDP 数据面",
			Value: "未监听（复用 tcp_listen 的 UDP over TCP）",
		})
	}
	rows = append(rows,
		relayOverviewRow{Key: "在线 agent", Value: strconv.Itoa(s.agents.Count())},
		relayOverviewRow{Key: "服务数", Value: strconv.Itoa(s.services.Len())},
		relayOverviewRow{Key: "配对占位", Value: strconv.Itoa(s.pairs.Len())},
	)
	if s.udpMode == protocol.UDPModeNative && s.udpTable != nil {
		rows = append(rows, relayOverviewRow{Key: "UDP 会话数", Value: strconv.Itoa(s.udpTable.Len())})
	} else {
		rows = append(rows, relayOverviewRow{Key: "UDP over TCP 会话数", Value: strconv.Itoa(s.carriers.len())})
	}
	return rows
}

// relayAgentRows 装配“在线 agent”表格。
func (s *server) relayAgentRows() []relayAgentRow {
	agents := s.agents.List()
	rows := make([]relayAgentRow, 0, len(agents))
	for _, agent := range agents {
		rows = append(rows, relayAgentRow{
			ID:          agent.ID,
			RemoteAddr:  agent.RemoteAddr,
			ConnectedAt: adminTime(agent.ConnectedAt),
			LastSeen:    adminTime(agent.LastSeen),
			Services:    strings.Join(agent.Services, ", "),
			Kick:        s.kickView("agent", agent.ID),
		})
	}
	return rows
}

// relayServiceRows 装配“服务注册表”表格。
func (s *server) relayServiceRows() []relayServiceRow {
	entries := s.services.List()
	rows := make([]relayServiceRow, 0, len(entries))
	for _, entry := range entries {
		info := s.serviceInfo(entry)
		rows = append(rows, relayServiceRow{
			ID:           entry.ID,
			Proto:        entry.Proto,
			Online:       info.Online,
			AgentID:      entry.AgentID,
			AgentAddr:    entry.AgentAddr,
			RegisteredAt: adminTime(entry.RegisteredAt),
			Kick:         s.kickView("agent", entry.AgentID),
		})
	}
	return rows
}

// relayPairRows 装配“当前配对表”。
func (s *server) relayPairRows() []relayPairRow {
	snaps := s.pairs.Snapshot()
	rows := make([]relayPairRow, 0, len(snaps))
	for _, snap := range snaps {
		key := snap.Key.String()
		rows = append(rows, relayPairRow{
			Key:           key,
			AgentArrived:  snap.AgentArrived,
			ClientArrived: snap.ClientArrived,
			Completed:     snap.Completed,
			Remaining:     snap.Remaining.Round(time.Millisecond).String(),
			Kick:          s.kickView("pair", key),
		})
	}
	return rows
}

// relayUDPSectionView 装配“UDP 会话表”（native 用 UDPTable，tcp_tunnel 用承载会话表）。
func (s *server) relayUDPSectionView() relayUDPSection {
	if s.udpMode == protocol.UDPModeNative && s.udpTable != nil {
		entries := s.udpTable.Snapshot()
		rows := make([]relayUDPSessionRow, 0, len(entries))
		for _, entry := range entries {
			rows = append(rows, relayUDPSessionRow{
				Key:        entry.Key.String(),
				AgentSide:  endpointText(entry.Agent),
				ClientSide: endpointText(entry.Client),
				Complete:   entry.Complete,
				CreatedAt:  adminTime(entry.CreatedAt),
				LastSeen:   adminTime(entry.LastSeen),
			})
		}
		return relayUDPSection{Native: true, Rows: rows, Empty: "当前没有 UDP 会话"}
	}

	entries := s.carriers.snapshot()
	rows := make([]relayUDPSessionRow, 0, len(entries))
	for _, entry := range entries {
		key := entry.Key.String()
		rows = append(rows, relayUDPSessionRow{
			Key:        key,
			AgentSide:  emptyDash(entry.AgentRemote),
			ClientSide: emptyDash(entry.ClientRemote),
			Complete:   entry.Full,
			CreatedAt:  adminTime(entry.CreatedAt),
			LastSeen:   adminTime(entry.LastSeen),
			Kick:       s.kickView("carrier", key),
		})
	}
	return relayUDPSection{Native: false, Rows: rows, Empty: "当前没有 UDP over TCP 会话"}
}

// handleKickAction 是管理页唯一的写操作：踢出 agent 控制连接 / 配对数据连接 /
// 承载连接。它只关闭既有连接句柄，让"关闭连接 → 转发退出 → 表项清理"的既有
// 路径统一处理，不在这里另写清理逻辑。
func (s *server) handleKickAction(w http.ResponseWriter, r *http.Request) {
	kind := r.PostFormValue("kind")
	id := r.PostFormValue("id")

	var ok bool
	switch kind {
	case "agent":
		ok = s.kickAgent(id)
	case "pair":
		ok = s.kickPair(id)
	case "carrier":
		ok = s.kickCarrier(id)
	}
	if !ok {
		http.Error(w, "未找到对应的连接或已失效", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "已踢出 %s：%s\n", kind, id)
}

// kickAgent 关闭 agent 的控制连接（既有清理路径负责注销服务与关闭数据连接）。
func (s *server) kickAgent(agentID string) bool {
	if agentID == "" {
		return false
	}
	tc := s.tunnel.agentConn(agentID)
	if tc == nil {
		// 注册表里可能还有残留条目（例如心跳判死竞态）。
		agent, ok := s.agents.Get(agentID)
		if !ok || agent.Control == nil {
			return false
		}
		s.log.Info("管理页踢出 agent（通过注册表连接）",
			"event", "kick",
			"agent_id", agentID,
			"remote", agent.RemoteAddr,
		)
		_ = agent.Control.Close()
		return true
	}
	_ = tc.writeTimeout(protocol.MsgKicked, &protocol.Kicked{Reason: "被管理页踢出"}, shutdownWriteTimeout)
	tc.close()
	s.log.Info("管理页踢出 agent",
		"event", "kick",
		"agent_id", agentID,
		"remote", tc.remoteAddr(),
	)
	return true
}

// kickPair 关闭一组配对连接（活动连接或待配对连接）。
func (s *server) kickPair(keyText string) bool {
	key, err := pairing.ParsePairingKey(keyText)
	if err != nil {
		return false
	}

	if ap, ok := s.takeActive(key); ok {
		s.log.Info("管理页踢出配对连接",
			"event", "kick",
			"pairing_key", key.String(),
			"agent_id", s.agentIDForService(key.ServiceID),
		)
		if ap.agent != nil {
			_ = ap.agent.Close()
		}
		if ap.client != nil {
			_ = ap.client.Close()
		}
		return true
	}

	if w := s.lookupWait(key); w != nil {
		if w.cancel() {
			s.pairs.Remove(key)
			s.log.Info("管理页踢出待配对连接",
				"event", "kick",
				"pairing_key", key.String(),
				"agent_id", s.agentIDForService(key.ServiceID),
			)
			return true
		}
	}

	if agent, client, ok := s.carriers.carriersFor(key); ok {
		s.log.Info("管理页踢出承载会话",
			"event", "kick",
			"pairing_key", key.String(),
			"agent_id", s.agentIDForService(key.ServiceID),
		)
		if agent != nil {
			agent.close()
		}
		if client != nil {
			client.close()
		}
		return true
	}
	return false
}

// kickCarrier 关闭承载会话所在的两条承载连接。
func (s *server) kickCarrier(keyText string) bool {
	key, err := pairing.ParsePairingKey(keyText)
	if err != nil {
		return false
	}
	agent, client, ok := s.carriers.carriersFor(key)
	if !ok {
		return false
	}
	s.log.Info("管理页踢出承载连接",
		"event", "kick",
		"pairing_key", key.String(),
		"agent_id", s.agentIDForService(key.ServiceID),
	)
	if agent != nil {
		agent.close()
	}
	if client != nil {
		client.close()
	}
	return true
}

// kickView 装配一个带框架 CSRF 令牌的踢出表单。
//
// 非回环只读模式下 Show=false（模板只渲染“只读”占位，写 Action 本身也会 403）。
func (s *server) kickView(kind, id string) relayKickView {
	view := relayKickView{Kind: kind, ID: id}
	if s.adminWritable() {
		view.Show = true
		// 每行一个一次性令牌：与旧实现一致，一页可连续踢出多个目标。
		view.Token = s.admin.CSRFToken()
	}
	return view
}

// adminWritable 报告 relay 管理页是否可写（未启动管理页时视为只读）。
func (s *server) adminWritable() bool {
	return s.admin != nil && s.admin.Writable()
}

// adminTime 格式化时间；零值显示为 "-"。
func adminTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

// adminBool 把布尔值显示为中文。
func adminBool(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

// endpointText 格式化 UDP 端点。
func endpointText(ep interface{ String() string }) string {
	if ep == nil {
		return "-"
	}
	return ep.String()
}

// emptyDash 空字符串显示为 "-"。
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
