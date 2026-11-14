package relay

import (
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

// renderAdmin 渲染 relay 管理页主体（只写 <section>，布局由 internal/admin 提供）。
func (s *server) renderAdmin(w io.Writer, _ *http.Request) {
	fmt.Fprint(w, `<section><h2>概览</h2><table><tbody>`)
	adminRow(w, "UDP 承载模式", s.udpMode)
	adminRow(w, "TCP 数据面", fmt.Sprintf("%s（端口 %d）", s.tcpAddr, s.tcpPort))
	if s.udpMode == protocol.UDPModeNative {
		adminRow(w, "UDP 数据面", fmt.Sprintf("%s（端口 %d）", s.udpAddr, s.udpPort))
	} else {
		adminRow(w, "UDP 数据面", "未监听（复用 tcp_listen 的 UDP over TCP）")
	}
	adminRow(w, "在线 agent", strconv.Itoa(s.agents.Count()))
	adminRow(w, "服务数", strconv.Itoa(s.services.Len()))
	adminRow(w, "配对占位", strconv.Itoa(s.pairs.Len()))
	if s.udpMode == protocol.UDPModeNative && s.udpTable != nil {
		adminRow(w, "UDP 会话数", strconv.Itoa(s.udpTable.Len()))
	} else {
		adminRow(w, "UDP over TCP 会话数", strconv.Itoa(s.carriers.len()))
	}
	fmt.Fprint(w, `</tbody></table></section>`)

	s.renderAdminAgents(w)
	s.renderAdminServices(w)
	s.renderAdminPairs(w)
	s.renderAdminUDPSessions(w)
}

// renderAdminAgents 渲染在线 agent 列表。
func (s *server) renderAdminAgents(w io.Writer) {
	agents := s.agents.List()
	fmt.Fprintf(w, `<section><h2>在线 agent（%d）</h2>`, len(agents))
	if len(agents) == 0 {
		fmt.Fprint(w, `<p class="empty">当前没有在线 agent</p></section>`)
		return
	}
	fmt.Fprint(w, `<table><thead><tr><th>Agent ID</th><th>对端地址</th><th>连接时间</th><th>最后活跃</th><th>服务</th><th>操作</th></tr></thead><tbody>`)
	for _, agent := range agents {
		fmt.Fprintf(w, `<tr><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(agent.ID),
			template.HTMLEscapeString(agent.RemoteAddr),
			adminTime(agent.ConnectedAt),
			adminTime(agent.LastSeen),
			template.HTMLEscapeString(strings.Join(agent.Services, ", ")),
			s.kickForm("agent", agent.ID),
		)
	}
	fmt.Fprint(w, `</tbody></table></section>`)
}

// renderAdminServices 渲染服务注册表。
func (s *server) renderAdminServices(w io.Writer) {
	entries := s.services.List()
	fmt.Fprintf(w, `<section><h2>服务注册表（%d）</h2>`, len(entries))
	if len(entries) == 0 {
		fmt.Fprint(w, `<p class="empty">还没有 agent 注册服务</p></section>`)
		return
	}
	fmt.Fprint(w, `<table><thead><tr><th>服务ID</th><th>协议</th><th>在线状态</th><th>所属 agent</th><th>Agent 地址</th><th>注册时间</th><th>操作</th></tr></thead><tbody>`)
	for _, entry := range entries {
		info := s.serviceInfo(entry)
		online := `<span class="dim">离线</span>`
		if info.Online {
			online = `<span class="tag">在线</span>`
		}
		fmt.Fprintf(w, `<tr><td class="mono">%s</td><td>%s</td><td>%s</td><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(entry.ID),
			template.HTMLEscapeString(entry.Proto),
			online,
			template.HTMLEscapeString(entry.AgentID),
			template.HTMLEscapeString(entry.AgentAddr),
			adminTime(entry.RegisteredAt),
			s.kickForm("agent", entry.AgentID),
		)
	}
	fmt.Fprint(w, `</tbody></table></section>`)
}

// renderAdminPairs 渲染待配对/已配对表。
func (s *server) renderAdminPairs(w io.Writer) {
	snaps := s.pairs.Snapshot()
	fmt.Fprintf(w, `<section><h2>当前配对表（%d）</h2>`, len(snaps))
	if len(snaps) == 0 {
		fmt.Fprint(w, `<p class="empty">当前没有配对占位</p></section>`)
		return
	}
	fmt.Fprint(w, `<table><thead><tr><th>配对键</th><th>Agent 已到</th><th>Client 已到</th><th>已完成</th><th>剩余</th><th>操作</th></tr></thead><tbody>`)
	for _, snap := range snaps {
		fmt.Fprintf(w, `<tr><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(snap.Key.String()),
			adminBool(snap.AgentArrived),
			adminBool(snap.ClientArrived),
			adminBool(snap.Completed),
			snap.Remaining.Round(time.Millisecond).String(),
			s.kickForm("pair", snap.Key.String()),
		)
	}
	fmt.Fprint(w, `</tbody></table></section>`)
}

// renderAdminUDPSessions 渲染 UDP 会话表（native 用 UDPTable，tcp_tunnel 用承载会话表）。
func (s *server) renderAdminUDPSessions(w io.Writer) {
	if s.udpMode == protocol.UDPModeNative && s.udpTable != nil {
		entries := s.udpTable.Snapshot()
		fmt.Fprintf(w, `<section><h2>UDP 会话表（native，%d）</h2>`, len(entries))
		if len(entries) == 0 {
			fmt.Fprint(w, `<p class="empty">当前没有 UDP 会话</p></section>`)
			return
		}
		fmt.Fprint(w, `<table><thead><tr><th>配对键</th><th>Agent 端点</th><th>Client 端点</th><th>状态</th><th>创建时间</th><th>最后活跃</th></tr></thead><tbody>`)
		for _, entry := range entries {
			status := "半开"
			if entry.Complete {
				status = "完整"
			}
			fmt.Fprintf(w, `<tr><td class="mono">%s</td><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				template.HTMLEscapeString(entry.Key.String()),
				template.HTMLEscapeString(endpointText(entry.Agent)),
				template.HTMLEscapeString(endpointText(entry.Client)),
				template.HTMLEscapeString(status),
				adminTime(entry.CreatedAt),
				adminTime(entry.LastSeen),
			)
		}
		fmt.Fprint(w, `</tbody></table></section>`)
		return
	}

	entries := s.carriers.snapshot()
	fmt.Fprintf(w, `<section><h2>UDP 会话表（UDP over TCP，%d）</h2>`, len(entries))
	if len(entries) == 0 {
		fmt.Fprint(w, `<p class="empty">当前没有 UDP over TCP 会话</p></section>`)
		return
	}
	fmt.Fprint(w, `<table><thead><tr><th>配对键</th><th>Agent 承载</th><th>Client 承载</th><th>状态</th><th>创建时间</th><th>最后活跃</th><th>操作</th></tr></thead><tbody>`)
	for _, entry := range entries {
		status := "半开"
		if entry.Full {
			status = "完整"
		}
		fmt.Fprintf(w, `<tr><td class="mono">%s</td><td class="mono">%s</td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(entry.Key.String()),
			template.HTMLEscapeString(emptyDash(entry.AgentRemote)),
			template.HTMLEscapeString(emptyDash(entry.ClientRemote)),
			template.HTMLEscapeString(status),
			adminTime(entry.CreatedAt),
			adminTime(entry.LastSeen),
			s.kickForm("carrier", entry.Key.String()),
		)
	}
	fmt.Fprint(w, `</tbody></table></section>`)
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

// kickForm 生成一个带框架 CSRF 令牌的踢出表单。
func (s *server) kickForm(kind, id string) string {
	return fmt.Sprintf(`<form class="inline" method="post" action="/action/kick">%s<input type="hidden" name="kind" value="%s"><input type="hidden" name="id" value="%s"><button type="submit" class="danger">踢出</button></form>`,
		string(s.admin.CSRFInput()),
		template.HTMLEscapeString(kind),
		template.HTMLEscapeString(id),
	)
}

// adminRow 写一行键值表格。
func adminRow(w io.Writer, key, value string) {
	fmt.Fprintf(w, `<tr><th style="width:180px">%s</th><td>%s</td></tr>`,
		template.HTMLEscapeString(key),
		template.HTMLEscapeString(value),
	)
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
