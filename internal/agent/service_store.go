package agent

import (
	"sync"

	"github.com/lsong98sh/pairlink/internal/config"
)

// serviceStore 是 agent 运行期可变的本地服务表（服务ID → 映射）。
//
// 阶段 7 起服务不再只从 cfg.Server 派生：管理页可以新增/修改/删除，
// 重连后重新注册的就是这张表的当前快照。读写都加锁，数据面 goroutine
// （handleIncoming）与管理页 handler 可并发访问。
type serviceStore struct {
	mu    sync.RWMutex
	items map[string]config.ServerService
	order []string
}

// newServiceStore 用初始服务列表构造服务表（保留顺序，主服务在前）。
func newServiceStore(list []config.ServerService) *serviceStore {
	s := &serviceStore{items: make(map[string]config.ServerService, len(list))}
	for _, svc := range list {
		s.setLocked(svc)
	}
	return s
}

// setLocked 写入服务并维护首次出现顺序（调用方持锁）。
func (s *serviceStore) setLocked(svc config.ServerService) {
	if _, exists := s.items[svc.ID]; !exists {
		s.order = append(s.order, svc.ID)
	}
	s.items[svc.ID] = svc
}

// get 返回指定服务。
func (s *serviceStore) get(id string) (config.ServerService, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	svc, ok := s.items[id]
	return svc, ok
}

// set 新增或更新服务。
func (s *serviceStore) set(svc config.ServerService) {
	s.mu.Lock()
	s.setLocked(svc)
	s.mu.Unlock()
}

// remove 删除服务（不存在时无操作）。
func (s *serviceStore) remove(id string) {
	s.mu.Lock()
	if _, exists := s.items[id]; exists {
		delete(s.items, id)
		for i, v := range s.order {
			if v == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
}

// snapshot 返回按稳定顺序排列的服务副本。
func (s *serviceStore) snapshot() []config.ServerService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]config.ServerService, 0, len(s.order))
	for _, id := range s.order {
		if svc, ok := s.items[id]; ok {
			out = append(out, svc)
		}
	}
	return out
}

// len 返回服务数量。
func (s *serviceStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}
