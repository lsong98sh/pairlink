package registry

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// 服务变更事件类型，与协议层 ServiceEvent.Event 取值一致。
const (
	// EventAdded 表示服务上线（首次注册）。
	EventAdded = protocol.EventAdded
	// EventRemoved 表示服务下线（注销或 agent 断开）。
	EventRemoved = protocol.EventRemoved
	// EventUpdated 表示服务信息更新（重复注册/在线状态变化）。
	EventUpdated = protocol.EventUpdated
)

// ServiceEntry 是服务注册表的一项。
type ServiceEntry struct {
	// ID 是服务ID（1..64，字符集 [a-z0-9-_]）。
	ID string
	// Proto 是协议类型 tcp / udp。
	Proto string
	// AgentID 是提供该服务的 agent 会话 ID。
	AgentID string
	// AgentAddr 是 agent 控制连接地址，用于管理页展示。
	AgentAddr string
	// Online 表示服务当前是否可接入（agent 在线且服务已注册）。
	Online bool
	// RegisteredAt 是注册时间。
	RegisteredAt time.Time
}

// Event 是一次服务注册表变更。
type Event struct {
	// Type 取 EventAdded / EventRemoved / EventUpdated。
	Type string
	// Service 是变更涉及的服务（Removed 时为被移除前的快照）。
	Service ServiceEntry
	// Revision 是变更后的注册表版本号。
	Revision uint64
	// At 是变更时间。
	At time.Time
}

// maxRecentOwners 是"最近断开归属"表的容量上限；超出后淘汰最旧记录（SEC-A2）。
const maxRecentOwners = 1024

// recentOwner 是某个服务名最后一个已知归属（含断开时间）。
//
// 在线表仍是"注销即删除"的原有语义；本表只在服务被移除/换主时留下痕迹，
// 供 relay 判定"异 owner 接管"是抢占（原 owner 在线）还是自愈（原 owner 已离线）。
type recentOwner struct {
	// AgentID 是最后一个注册该服务的 agent 会话 ID。
	AgentID string
	// At 是记录时间（服务被注销或改属的时刻）。
	At time.Time
}

// ServiceRegistry 是并发安全的服务注册表，键为服务ID。
type ServiceRegistry struct {
	mu        sync.RWMutex
	services  map[string]ServiceEntry
	recent    map[string]recentOwner
	revision  uint64
	subs      map[int]func(Event)
	nextSubID int
	now       func() time.Time
}

// NewServiceRegistry 创建空的服务注册表。
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string]ServiceEntry),
		recent:   make(map[string]recentOwner),
		subs:     make(map[int]func(Event)),
		now:      time.Now,
	}
}

// SetClock 替换时间源，仅用于测试。传入 nil 恢复 time.Now。
func (r *ServiceRegistry) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	r.now = now
}

// Register 注册或更新一个服务。
//
// 首次注册产生 EventAdded，同一服务ID 再次注册产生 EventUpdated；
// ID 为空返回错误。RegisteredAt 为零时自动填当前时间。
func (r *ServiceRegistry) Register(e ServiceEntry) error {
	if e.ID == "" {
		return fmt.Errorf("registry: 服务ID 不能为空")
	}

	r.mu.Lock()
	now := r.now()
	if e.RegisteredAt.IsZero() {
		e.RegisteredAt = now
	}
	old, existed := r.services[e.ID]
	// 换主时保留旧归属痕迹（跨 owner 覆盖只可能发生在 relay 已判定"原 owner
	// 离线、允许接管"之后），供审计/管理页显示"最近归属"。
	if existed && old.AgentID != "" && old.AgentID != e.AgentID {
		r.rememberLocked(e.ID, old.AgentID, now)
	}
	r.services[e.ID] = e
	r.revision++
	rev := r.revision
	subs := r.subscribersLocked()
	r.mu.Unlock()

	evType := EventAdded
	if existed {
		evType = EventUpdated
	}
	r.dispatch(subs, Event{Type: evType, Service: e, Revision: rev, At: now})
	return nil
}

// Unregister 注销服务。
//
// agentID 非空时要求与注册者一致，否则返回 ErrServiceOwnerMismatch；
// 服务不存在返回 ErrServiceNotFound。
func (r *ServiceRegistry) Unregister(serviceID, agentID string) error {
	r.mu.Lock()
	e, ok := r.services[serviceID]
	if !ok {
		r.mu.Unlock()
		return ErrServiceNotFound
	}
	if agentID != "" && e.AgentID != agentID {
		r.mu.Unlock()
		return ErrServiceOwnerMismatch
	}
	delete(r.services, serviceID)
	now := r.now()
	if e.AgentID != "" {
		r.rememberLocked(serviceID, e.AgentID, now)
	}
	r.revision++
	rev := r.revision
	subs := r.subscribersLocked()
	r.mu.Unlock()

	r.dispatch(subs, Event{Type: EventRemoved, Service: e, Revision: rev, At: now})
	return nil
}

// rememberLocked 记录某服务名的最近归属；容量满时淘汰最旧记录。
//
// 调用方必须持有 r.mu。重复记录同一服务即刷新时间并保留最新 owner。
func (r *ServiceRegistry) rememberLocked(serviceID, agentID string, at time.Time) {
	if serviceID == "" || agentID == "" {
		return
	}
	if r.recent == nil {
		r.recent = make(map[string]recentOwner)
	}
	if _, exists := r.recent[serviceID]; !exists && len(r.recent) >= maxRecentOwners {
		oldestID := ""
		var oldestAt time.Time
		for id, rec := range r.recent {
			if oldestID == "" || rec.At.Before(oldestAt) {
				oldestID, oldestAt = id, rec.At
			}
		}
		delete(r.recent, oldestID)
	}
	r.recent[serviceID] = recentOwner{AgentID: agentID, At: at}
}

// RecentOwner 返回某服务名最后一个已知归属与记录时间。
//
// ok=false 表示没有历史记录（从未注册过，或记录已被容量淘汰）。
// 该表只用于 relay 的归属判定与审计，不参与在线服务列表。
func (r *ServiceRegistry) RecentOwner(serviceID string) (agentID string, at time.Time, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.recent[serviceID]
	if !ok {
		return "", time.Time{}, false
	}
	return rec.AgentID, rec.At, true
}

// RecentOwnerCount 返回"最近归属"表的记录数（管理页/测试用）。
func (r *ServiceRegistry) RecentOwnerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.recent)
}

// RememberOwner 补记某服务名的最近归属（at 为零时取当前时间）。
//
// relay 在"在线表仍有记录但历史表没有"的接管路径上补一条，保证 service_takeover
// 事件与审计有来源。容量语义与 Unregister 的记录一致。
func (r *ServiceRegistry) RememberOwner(serviceID, agentID string, at time.Time) {
	if at.IsZero() {
		at = r.now()
	}
	r.mu.Lock()
	r.rememberLocked(serviceID, agentID, at)
	r.mu.Unlock()
}

// UnregisterAgent 注销某个 agent 的全部服务，返回被移除的服务ID（升序）。
func (r *ServiceRegistry) UnregisterAgent(agentID string) []string {
	r.mu.RLock()
	ids := make([]string, 0, len(r.services))
	for id, e := range r.services {
		if e.AgentID == agentID {
			ids = append(ids, id)
		}
	}
	r.mu.RUnlock()
	sort.Strings(ids)

	removed := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := r.Unregister(id, agentID); err == nil {
			removed = append(removed, id)
		}
	}
	return removed
}

// Get 返回指定服务的快照。
func (r *ServiceRegistry) Get(serviceID string) (ServiceEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.services[serviceID]
	return e, ok
}

// List 返回全部服务，按服务ID 升序。
func (r *ServiceRegistry) List() []ServiceEntry {
	r.mu.RLock()
	out := make([]ServiceEntry, 0, len(r.services))
	for _, e := range r.services {
		out = append(out, e)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListByAgent 返回指定 agent 的服务，按服务ID 升序。
func (r *ServiceRegistry) ListByAgent(agentID string) []ServiceEntry {
	r.mu.RLock()
	out := make([]ServiceEntry, 0, 4)
	for _, e := range r.services {
		if e.AgentID == agentID {
			out = append(out, e)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Revision 返回当前注册表版本号（每次变更自增）。
func (r *ServiceRegistry) Revision() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

// Len 返回服务数量。
func (r *ServiceRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.services)
}

// Subscribe 订阅服务变更，返回取消函数（可重复调用）。
//
// 回调在锁外串行执行，实现必须快速返回（重活请投递到自己的 channel）。
func (r *ServiceRegistry) Subscribe(fn func(Event)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	r.mu.Lock()
	id := r.nextSubID
	r.nextSubID++
	r.subs[id] = fn
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			r.mu.Unlock()
		})
	}
}

// subscribersLocked 复制订阅者列表，调用方需持有锁。
func (r *ServiceRegistry) subscribersLocked() []func(Event) {
	out := make([]func(Event), 0, len(r.subs))
	for _, fn := range r.subs {
		out = append(out, fn)
	}
	return out
}

// dispatch 在锁外依次调用订阅回调。
func (r *ServiceRegistry) dispatch(subs []func(Event), ev Event) {
	for _, fn := range subs {
		fn(ev)
	}
}
