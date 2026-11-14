package agent

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/version"
)

// startAdmin 在 [common] manage_listen 非空时启动只读管理页。
//
// 页面展示 spec §5 / docs/contract.md §3.8 要求的三块内容：
//   - 本机已注册服务（service_id / proto / 本地目标）；
//   - 到 relay 的控制连接状态（地址、连接时长、udp_mode、心跳）；
//   - 当前活跃数据连接（配对键、协议、载体、远端、建立时间、收发字节）。
//
// 第一版只读，不注册任何 Action。
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

// renderAdmin 渲染管理页主体（不含 <html>/<head>/<body>，由框架套布局）。
func (a *agent) renderAdmin(w io.Writer, r *http.Request) {
	var b bytes.Buffer
	a.renderServices(&b)
	a.renderControl(&b)
	a.renderSessions(&b)
	_, _ = io.WriteString(w, b.String())
}

// renderServices 渲染本机已注册服务列表。
func (a *agent) renderServices(b *bytes.Buffer) {
	registered := a.registeredServices()
	b.WriteString(`<section><h2>本机已注册服务</h2>`)
	if len(registered) == 0 {
		b.WriteString(`<p class="empty">尚未注册任何服务</p></section>`)
		return
	}
	b.WriteString(`<table><thead><tr><th>服务ID</th><th>协议</th><th>本地目标</th><th>udp_mode</th><th>状态</th></tr></thead><tbody>`)
	for _, svc := range registered {
		fmt.Fprintf(b, `<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td>%s</td><td>已注册</td></tr>`,
			esc(svc.Service.ID),
			esc(svc.Service.Proto),
			esc(svc.Service.Addr),
			esc(svc.UDPMode),
		)
	}
	b.WriteString(`</tbody></table></section>`)
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
