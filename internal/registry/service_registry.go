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

// ServiceRegistry 是并发安全的服务注册表，键为服务ID。
type ServiceRegistry struct {
	mu        sync.RWMutex
	services  map[string]ServiceEntry
	revision  uint64
	subs      map[int]func(Event)
	nextSubID int
	now       func() time.Time
}

// NewServiceRegistry 创建空的服务注册表。
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string]ServiceEntry),
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
	_, existed := r.services[e.ID]
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
	r.revision++
	rev := r.revision
	now := r.now()
	subs := r.subscribersLocked()
	r.mu.Unlock()

	r.dispatch(subs, Event{Type: EventRemoved, Service: e, Revision: rev, At: now})
	return nil
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
