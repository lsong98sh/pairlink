package agent

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// localSession 是 agent 侧一次配对会话的本地记录。
//
// 它承担三个职责：
//  1. 关停/超时清理时集中关闭该会话持有的全部 socket（track/closeAll）；
//  2. 为管理页提供快照（配对键、协议、载体、远端、时长、收发字节）；
//  3. 记录最后一次活动时间，供 UDP 空闲清理使用。
//
// 生命周期：handleIncoming 登记 → 各数据面 handler 通过 track 登记 socket、
// markReady 标记可用 → handler 返回时 endSession 关连接并删表项。
type localSession struct {
	key      pairing.PairingKey
	proto    string
	upstream string

	startedAt time.Time
	readyAt   time.Time

	mu      sync.Mutex
	carrier string
	remote  string
	ready   bool
	closed  bool
	closers []io.Closer

	// lastSeen 是最后一次活动时间（UnixNano，0 表示未记录）。
	lastSeen atomic.Int64
	// bytesIn 是 relay → 本地目标的字节数（TCP 为字节，UDP 为数据报载荷字节）。
	bytesIn atomic.Int64
	// bytesOut 是本地目标 → relay 的字节数。
	bytesOut atomic.Int64
}

// newLocalSession 创建一个本地会话记录。
func newLocalSession(key pairing.PairingKey, proto, upstream string) *localSession {
	s := &localSession{key: key, proto: proto, upstream: upstream, startedAt: time.Now()}
	s.touch()
	return s
}

// track 把一个需随会话关闭的句柄登记进来。
//
// 会话已关闭时立即关闭该句柄并返回 false，调用方应停止后续操作。
func (s *localSession) track(c io.Closer) bool {
	if c == nil {
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return false
	}
	s.closers = append(s.closers, c)
	s.mu.Unlock()
	return true
}

// closeAll 关闭该会话持有的全部句柄（幂等，可在任意 goroutine 调用）。
//
// 只关闭连接、不删除表项：表项由会话自身的 goroutine 退出时 endSession 删除，
// 以保持 docs/architecture.md §5 的「关连接 → 等转发返回 → 删表项」顺序。
func (s *localSession) closeAll() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()
	for _, c := range closers {
		_ = c.Close()
	}
}

// isClosed 报告会话是否已被关闭。
func (s *localSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// markReady 标记会话的数据面已建立。
func (s *localSession) markReady() {
	s.mu.Lock()
	if !s.ready {
		s.ready = true
		s.readyAt = time.Now()
	}
	s.mu.Unlock()
}

// isReady 报告数据面是否已建立。
func (s *localSession) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// setCarrier 记录承载方式。
func (s *localSession) setCarrier(v string) {
	s.mu.Lock()
	s.carrier = v
	s.mu.Unlock()
}

// carrierName 返回承载方式（未知时返回 "tcp"）。
func (s *localSession) carrierName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carrier == "" {
		return carrierTCP
	}
	return s.carrier
}

// setRemote 记录数据面对端地址。
func (s *localSession) setRemote(v string) {
	s.mu.Lock()
	s.remote = v
	s.mu.Unlock()
}

// touch 刷新最后一次活动时间。
func (s *localSession) touch() {
	s.lastSeen.Store(time.Now().UnixNano())
}

// lastSeenTime 返回最后一次活动时间。
func (s *localSession) lastSeenTime() time.Time {
	return time.Unix(0, s.lastSeen.Load())
}

// sessionSnapshot 是会话的只读快照（管理页展示用）。
type sessionSnapshot struct {
	Key           string
	Proto         string
	Carrier       string
	Remote        string
	Upstream      string
	StartedAt     time.Time
	EstablishedAt time.Time
	Ready         bool
	BytesIn       int64
	BytesOut      int64
	IdleFor       time.Duration
}

// snapshot 返回会话快照。
func (s *localSession) snapshot() sessionSnapshot {
	s.mu.Lock()
	snap := sessionSnapshot{
		Key:           s.key.String(),
		Proto:         s.proto,
		Carrier:       s.carrier,
		Remote:        s.remote,
		Upstream:      s.upstream,
		StartedAt:     s.startedAt,
		EstablishedAt: s.readyAt,
		Ready:         s.ready,
	}
	s.mu.Unlock()
	if snap.Carrier == "" {
		snap.Carrier = carrierTCP
	}
	snap.BytesIn = s.bytesIn.Load()
	snap.BytesOut = s.bytesOut.Load()
	snap.IdleFor = time.Since(s.lastSeenTime())
	return snap
}

// sessionTable 是 agent 的活跃会话表（配对键 → 本地会话）。
type sessionTable struct {
	mu    sync.Mutex
	items map[pairing.PairingKey]*localSession
}

// newSessionTable 创建空的会话表。
func newSessionTable() *sessionTable {
	return &sessionTable{items: make(map[pairing.PairingKey]*localSession)}
}

// add 登记一个会话；同一配对键已有活跃会话时返回 errSessionExists。
func (t *sessionTable) add(s *localSession) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.items[s.key]; ok {
		return errSessionExists
	}
	t.items[s.key] = s
	return nil
}

// get 返回指定配对键的会话。
func (t *sessionTable) get(key pairing.PairingKey) (*localSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.items[key]
	return s, ok
}

// remove 移除会话（仅当表中仍是同一个对象，幂等）。
func (t *sessionTable) remove(s *localSession) {
	if s == nil {
		return
	}
	t.mu.Lock()
	if cur, ok := t.items[s.key]; ok && cur == s {
		delete(t.items, s.key)
	}
	t.mu.Unlock()
}

// len 返回当前会话数量。
func (t *sessionTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}

// closeAll 关闭全部会话持有的连接（不删表项）。
func (t *sessionTable) closeAll() {
	t.mu.Lock()
	sessions := make([]*localSession, 0, len(t.items))
	for _, s := range t.items {
		sessions = append(sessions, s)
	}
	t.mu.Unlock()
	for _, s := range sessions {
		s.closeAll()
	}
}

// snapshot 返回按配对键排序的会话快照。
func (t *sessionTable) snapshot() []sessionSnapshot {
	t.mu.Lock()
	out := make([]sessionSnapshot, 0, len(t.items))
	for _, s := range t.items {
		out = append(out, s.snapshot())
	}
	t.mu.Unlock()
	// 排序保证管理页输出稳定（小规模，插入排序足够）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Key < out[j-1].Key; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// sweepIdle 关闭空闲超过 timeout 的 UDP 会话，返回被关闭的配对键。
//
// 只关闭连接，由会话自身的 goroutine 退出时删除表项（保持清理顺序）。
func (t *sessionTable) sweepIdle(now time.Time, timeout time.Duration) []pairing.PairingKey {
	if timeout <= 0 {
		return nil
	}
	var expired []*localSession
	t.mu.Lock()
	for _, s := range t.items {
		if s.proto != "udp" {
			continue
		}
		if now.Sub(s.lastSeenTime()) >= timeout {
			expired = append(expired, s)
		}
	}
	t.mu.Unlock()

	keys := make([]pairing.PairingKey, 0, len(expired))
	for _, s := range expired {
		keys = append(keys, s.key)
		s.closeAll()
	}
	return keys
}

// sweepLoop 是唯一的本地 UDP 会话空闲清理 ticker。
func (a *agent) sweepLoop() {
	timeout := a.udpSessionTimeout()
	interval := timeout / 4
	if interval < minSessionSweepInterval {
		interval = minSessionSweepInterval
	}
	if interval > maxSessionSweepInterval {
		interval = maxSessionSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-ticker.C:
			for _, key := range a.sessions.sweepIdle(now, timeout) {
				a.log.Warn("UDP 会话空闲超时，关闭并清理", "event", "udp_evict",
					"pairing_key", key.String(), "detail", "timeout="+timeout.String())
			}
		}
	}
}
