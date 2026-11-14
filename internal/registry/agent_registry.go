package registry

import (
	"sort"
	"sync"
	"time"
)

// AgentRegistry 是并发安全的在线 agent 表。
type AgentRegistry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

// NewAgentRegistry 创建空的 agent 表。
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{agents: make(map[string]*Agent)}
}

// Add 登记一个在线 agent（存储副本，调用方之后修改入参不会影响注册表）。
//
// ID 为空或已存在时返回错误。
func (r *AgentRegistry) Add(a *Agent) error {
	if a == nil {
		return ErrAgentNotFound
	}
	if a.ID == "" {
		return ErrAgentNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[a.ID]; exists {
		return ErrAgentExists
	}
	cp := *a
	cp.Services = append([]string(nil), a.Services...)
	now := time.Now()
	if cp.ConnectedAt.IsZero() {
		cp.ConnectedAt = now
	}
	if cp.LastSeen.IsZero() {
		cp.LastSeen = cp.ConnectedAt
	}
	r.agents[a.ID] = &cp
	return nil
}

// Remove 移除 agent；不存在时返回 ErrAgentNotFound。
func (r *AgentRegistry) Remove(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[agentID]; !ok {
		return ErrAgentNotFound
	}
	delete(r.agents, agentID)
	return nil
}

// Get 返回 agent 快照。
func (r *AgentRegistry) Get(agentID string) (Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[agentID]
	if !ok {
		return Agent{}, false
	}
	return snapshotAgent(a), true
}

// List 返回全部 agent 快照，按 ID 升序。
func (r *AgentRegistry) List() []Agent {
	r.mu.RLock()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, snapshotAgent(a))
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Count 返回在线 agent 数量。
func (r *AgentRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}

// Touch 更新 agent 的最后活跃时间；agent 不存在时返回 ErrAgentNotFound。
func (r *AgentRegistry) Touch(agentID string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[agentID]
	if !ok {
		return ErrAgentNotFound
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.LastSeen = now
	return nil
}

// SetServices 更新 agent 的服务ID 列表；agent 不存在时返回 ErrAgentNotFound。
func (r *AgentRegistry) SetServices(agentID string, services []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[agentID]
	if !ok {
		return ErrAgentNotFound
	}
	a.Services = append([]string(nil), services...)
	return nil
}

// snapshotAgent 复制 agent（含 Services 切片），调用方需持有锁。
func snapshotAgent(a *Agent) Agent {
	cp := *a
	cp.Services = append([]string(nil), a.Services...)
	return cp
}
