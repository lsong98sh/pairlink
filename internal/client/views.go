package client

import (
	"fmt"
	"sort"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// listenerView 是管理页「本地端口映射表」的一行。
type listenerView struct {
	ServiceID string
	Proto     string
	Listen    string // ini 中配置的监听地址
	Addr      string // 实际绑定地址
}

// pairView 是管理页「当前活跃配对」的一行。
type pairView struct {
	Key     string
	Proto   string
	Started time.Time
	Status  string
	Remote  string
	ToRelay int64
	ToLocal int64
}

// servicesView 返回服务列表视图的快照与当前 revision。
func (c *client) servicesView() (map[string]protocol.ServiceInfo, uint64) {
	c.svcMu.RLock()
	defer c.svcMu.RUnlock()
	out := make(map[string]protocol.ServiceInfo, len(c.services))
	for k, v := range c.services {
		out[k] = v
	}
	return out, c.revision
}

// applyServiceList 用 MsgServiceList 整体替换本地服务视图（revision 取更大值）。
//
// revision 比本地已见版本更旧的快照**整份忽略**：relay 的订阅基线与并发事件可能短暂
// 出现"全量列表比刚应用的事件更旧"的情形，用旧快照替换会丢掉刚应用的变更。
func (c *client) applyServiceList(list *protocol.ServiceList) {
	if list == nil {
		return
	}
	c.svcMu.Lock()
	defer c.svcMu.Unlock()
	if list.Revision < c.revision {
		c.log.Debug("忽略旧的全量服务列表", "event", "service_list",
			"detail", fmt.Sprintf("list_revision=%d seen=%d", list.Revision, c.revision))
		return
	}
	services := make(map[string]protocol.ServiceInfo, len(list.Services))
	for _, svc := range list.Services {
		services[svc.ServiceID] = svc
	}
	c.services = services
	if list.Revision > c.revision {
		c.revision = list.Revision
	}
}

// applyServiceEvent 用 MsgServiceEvent 增量更新服务视图。
//
// revision 单调：revision <= 已见值的旧/重复事件直接丢弃（docs/protocol.md §4.5）。
func (c *client) applyServiceEvent(ev *protocol.ServiceEvent) {
	if ev == nil {
		return
	}

	c.svcMu.Lock()
	if ev.Revision <= c.revision {
		c.svcMu.Unlock()
		c.log.Debug("丢弃旧的服务列表事件", "event", "service_event",
			"service_id", ev.Service.ServiceID,
			"detail", fmt.Sprintf("event=%s revision=%d seen=%d", ev.Event, ev.Revision, c.revision))
		return
	}
	switch ev.Event {
	case protocol.EventAdded, protocol.EventUpdated:
		c.services[ev.Service.ServiceID] = ev.Service
	case protocol.EventRemoved:
		delete(c.services, ev.Service.ServiceID)
	default:
		c.svcMu.Unlock()
		c.log.Warn("未知的服务列表事件类型，忽略", "event", "service_event",
			"service_id", ev.Service.ServiceID, "detail", "event="+ev.Event)
		return
	}
	c.revision = ev.Revision
	c.svcMu.Unlock()

	c.log.Info("服务列表已更新", "event", "service_event", "service_id", ev.Service.ServiceID,
		"detail", fmt.Sprintf("event=%s proto=%s online=%t revision=%d",
			ev.Event, ev.Service.Proto, ev.Service.Online, ev.Revision))

	// 第一版不热切换本地监听：订阅到 proto 变化只记 WARN。
	if ev.Event == protocol.EventAdded || ev.Event == protocol.EventUpdated {
		if p := c.listenerProto(ev.Service.ServiceID); p != "" && p != ev.Service.Proto {
			c.log.Warn("服务的协议与本地监听不一致（第一版不热切换，保持现状）", "event", "service_proto_mismatch",
				"service_id", ev.Service.ServiceID,
				"detail", fmt.Sprintf("listen_proto=%s relay_proto=%s", p, ev.Service.Proto))
		}
	}
}

// listenerViews 返回本地端口映射表快照。
func (c *client) listenerViews() []listenerView {
	c.listenerMu.Lock()
	listeners := append([]*localListener(nil), c.listeners...)
	c.listenerMu.Unlock()

	out := make([]listenerView, 0, len(listeners))
	for _, l := range listeners {
		out = append(out, listenerView{
			ServiceID: l.svcID,
			Proto:     l.proto,
			Listen:    l.listen,
			Addr:      l.actualAddr(),
		})
	}
	return out
}

// pairViews 汇总 TCP 连接表与 UDP 会话表，返回当前活跃配对快照（按配对键排序）。
func (c *client) pairViews() []pairView {
	var out []pairView

	c.tcpMu.Lock()
	tcpList := make([]*tcpSession, 0, len(c.tcpSessions))
	for _, s := range c.tcpSessions {
		tcpList = append(tcpList, s)
	}
	c.tcpMu.Unlock()
	for _, s := range tcpList {
		key, proto, state, started, remote, toRelay, toLocal := s.snapshot()
		if state == stateClosed {
			continue
		}
		out = append(out, pairView{
			Key:     key,
			Proto:   protoOr(proto, "tcp"),
			Started: started,
			Status:  state.statusText(),
			Remote:  remote,
			ToRelay: toRelay,
			ToLocal: toLocal,
		})
	}

	c.udpMu.Lock()
	udpList := make([]*udpSession, 0, len(c.udpSessions))
	for _, s := range c.udpSessions {
		udpList = append(udpList, s)
	}
	c.udpMu.Unlock()
	for _, s := range udpList {
		key, proto, state, started, remote, toRelay, toLocal := s.snapshot()
		if state == stateClosed {
			continue
		}
		out = append(out, pairView{
			Key:     key,
			Proto:   protoOr(proto, "udp"),
			Started: started,
			Status:  state.statusText(),
			Remote:  remote,
			ToRelay: toRelay,
			ToLocal: toLocal,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// protoOr 在配对结果尚未到达时按本地监听协议展示。
func protoOr(proto, fallback string) string {
	if proto == "" {
		return fallback
	}
	return proto
}

// sessionCount 返回本地 TCP 连接数（测试与管理页辅助）。
func (c *client) sessionCount() int {
	c.tcpMu.Lock()
	defer c.tcpMu.Unlock()
	return len(c.tcpSessions)
}

// udpSessionCount 返回本地 UDP 会话数（测试与管理页辅助）。
func (c *client) udpSessionCount() int {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	return len(c.udpSessions)
}
