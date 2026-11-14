package registry

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// UDPEndpoint 是 UDP 数据面的一个对端端点（IP + 端口）。
type UDPEndpoint struct {
	// IP 是 IP 地址（IPv4 或 IPv6）。
	IP netip.Addr
	// Port 是端口（1..65535）。
	Port uint16
}

// IsValid 报告端点是否可用。
func (e UDPEndpoint) IsValid() bool { return e.IP.IsValid() && e.Port != 0 }

// AddrPort 把端点转换为 netip.AddrPort（用作 map 键）。
func (e UDPEndpoint) AddrPort() netip.AddrPort {
	return netip.AddrPortFrom(e.IP, e.Port)
}

// String 返回 "ip:port"。
func (e UDPEndpoint) String() string {
	if !e.IsValid() {
		return "<invalid>"
	}
	return e.AddrPort().String()
}

// UDPEndpointFromAddrPort 由 netip.AddrPort 构造端点。
func UDPEndpointFromAddrPort(ap netip.AddrPort) UDPEndpoint {
	return UDPEndpoint{IP: ap.Addr(), Port: ap.Port()}
}

// UDPEndpointFromString 解析 "ip:port" 形式的端点。
func UDPEndpointFromString(s string) (UDPEndpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return UDPEndpoint{}, fmt.Errorf("registry: 解析 UDP 端点 %q: %w", s, err)
	}
	return UDPEndpointFromAddrPort(ap), nil
}

// UDPEndpointFromNetAddr 由 net.Addr 构造端点（通常传 *net.UDPAddr）。
func UDPEndpointFromNetAddr(a net.Addr) (UDPEndpoint, error) {
	if a == nil {
		return UDPEndpoint{}, ErrUDPEndpointInvalid
	}
	switch v := a.(type) {
	case *net.UDPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return UDPEndpoint{}, fmt.Errorf("registry: 非法 IP: %v", v.IP)
		}
		return UDPEndpoint{IP: ip.Unmap(), Port: uint16(v.Port)}, nil
	case interface{ String() string }:
		return UDPEndpointFromString(v.String())
	default:
		return UDPEndpoint{}, fmt.Errorf("registry: 不支持的地址类型 %T", a)
	}
}

// UDPEntry 是 UDP 会话表的一行快照。
type UDPEntry struct {
	// Key 是配对键。
	Key pairing.PairingKey
	// Agent 是 agent 侧端点。
	Agent UDPEndpoint
	// Client 是 client 侧端点。
	Client UDPEndpoint
	// AgentReady 表示 agent 侧已报到。
	AgentReady bool
	// ClientReady 表示 client 侧已报到。
	ClientReady bool
	// Complete 表示两侧都已报到，可以转发。
	Complete bool
	// CreatedAt 是会话创建时间。
	CreatedAt time.Time
	// LastSeen 是最后一次活动时间。
	LastSeen time.Time
}

// udpSession 是内部会话结构。
type udpSession struct {
	key         pairing.PairingKey
	agent       UDPEndpoint
	client      UDPEndpoint
	agentReady  bool
	clientReady bool
	createdAt   time.Time
	lastSeen    time.Time
}

// complete 报告两侧是否都已报到。
func (s *udpSession) complete() bool { return s.agentReady && s.clientReady }

// snapshot 复制为外部快照。
func (s *udpSession) snapshot() UDPEntry {
	return UDPEntry{
		Key:         s.key,
		Agent:       s.agent,
		Client:      s.client,
		AgentReady:  s.agentReady,
		ClientReady: s.clientReady,
		Complete:    s.complete(),
		CreatedAt:   s.createdAt,
		LastSeen:    s.lastSeen,
	}
}

// UDPTable 是并发安全的 UDP 会话表，同时维护 key → 会话与 endpoint → key 两个索引。
type UDPTable struct {
	mu         sync.Mutex
	sessions   map[pairing.PairingKey]*udpSession
	byEndpoint map[netip.AddrPort]pairing.PairingKey
	now        func() time.Time
}

// NewUDPTable 创建空的 UDP 会话表。
func NewUDPTable() *UDPTable {
	return &UDPTable{
		sessions:   make(map[pairing.PairingKey]*udpSession),
		byEndpoint: make(map[netip.AddrPort]pairing.PairingKey),
		now:        time.Now,
	}
}

// SetClock 替换时间源，仅用于测试。传入 nil 恢复 time.Now。
func (t *UDPTable) SetClock(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	t.now = now
}

// RegisterHalf 登记一侧的 UDP 端点。
//
// 返回 full 表示两侧都已报到（会话完整）；peer 是已报到对端的端点，
// 对端尚未报到时为零值。幂等：同一侧用同一端点重复报到返回同样结果。
//
// 错误：
//   - 端点非法：ErrUDPEndpointInvalid；
//   - 同一侧更换端点：ErrUDPEndpointConflict；
//   - 端点已被其它配对键占用：ErrUDPEndpointConflict。
func (t *UDPTable) RegisterHalf(key pairing.PairingKey, side pairing.Side, ep UDPEndpoint) (full bool, peer UDPEndpoint, err error) {
	if !side.Valid() {
		return false, UDPEndpoint{}, pairing.ErrInvalidSide
	}
	if !ep.IsValid() {
		return false, UDPEndpoint{}, ErrUDPEndpointInvalid
	}
	ep.IP = ep.IP.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	s, ok := t.sessions[key]
	if !ok {
		if owner, taken := t.byEndpoint[ep.AddrPort()]; taken && owner != key {
			return false, UDPEndpoint{}, fmt.Errorf("%w: %s 已属于 %s", ErrUDPEndpointConflict, ep, owner)
		}
		s = &udpSession{key: key, createdAt: now, lastSeen: now}
		if side == pairing.SideAgent {
			s.agent, s.agentReady = ep, true
		} else {
			s.client, s.clientReady = ep, true
		}
		t.sessions[key] = s
		t.byEndpoint[ep.AddrPort()] = key
		return s.complete(), t.peerLocked(s, side), nil
	}

	cur, ready := t.endpointsLocked(s, side)
	if ready {
		if cur != ep {
			return false, UDPEndpoint{}, fmt.Errorf("%w: 配对键 %s 的 %s 侧端点已固定为 %s", ErrUDPEndpointConflict, key, side, cur)
		}
		return s.complete(), t.peerLocked(s, side), nil
	}
	if owner, taken := t.byEndpoint[ep.AddrPort()]; taken && owner != key {
		return false, UDPEndpoint{}, fmt.Errorf("%w: %s 已属于 %s", ErrUDPEndpointConflict, ep, owner)
	}

	if side == pairing.SideAgent {
		s.agent, s.agentReady = ep, true
	} else {
		s.client, s.clientReady = ep, true
	}
	t.byEndpoint[ep.AddrPort()] = key
	s.lastSeen = now
	return s.complete(), t.peerLocked(s, side), nil
}

// endpointsLocked 返回指定侧已登记的端点及是否已登记，调用方需持有锁。
func (t *UDPTable) endpointsLocked(s *udpSession, side pairing.Side) (UDPEndpoint, bool) {
	if side == pairing.SideAgent {
		return s.agent, s.agentReady
	}
	return s.client, s.clientReady
}

// peerLocked 返回 side 对端的端点，对端未报到时为零值，调用方需持有锁。
func (t *UDPTable) peerLocked(s *udpSession, side pairing.Side) UDPEndpoint {
	if side == pairing.SideAgent {
		return s.client
	}
	return s.agent
}

// LookupByEndpoint 按源端点反查配对键与对端端点。
//
// ok 表示该端点已登记；peer 为零值表示对端尚未报到（此时不可转发）。
func (t *UDPTable) LookupByEndpoint(ep UDPEndpoint) (key pairing.PairingKey, peer UDPEndpoint, ok bool) {
	if !ep.IsValid() {
		return pairing.PairingKey{}, UDPEndpoint{}, false
	}
	ep.IP = ep.IP.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	k, found := t.byEndpoint[ep.AddrPort()]
	if !found {
		return pairing.PairingKey{}, UDPEndpoint{}, false
	}
	s := t.sessions[k]
	if s == nil {
		return pairing.PairingKey{}, UDPEndpoint{}, false
	}
	if s.agentReady && s.agent == ep {
		return k, s.client, true
	}
	return k, s.agent, true
}

// LookupByKey 返回配对键对应的会话快照。
func (t *UDPTable) LookupByKey(key pairing.PairingKey) (UDPEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[key]
	if !ok {
		return UDPEntry{}, false
	}
	return s.snapshot(), true
}

// Touch 更新会话最后活动时间；会话不存在时忽略。
func (t *UDPTable) Touch(key pairing.PairingKey, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[key]
	if !ok {
		return
	}
	if at.IsZero() {
		at = t.now()
	}
	s.lastSeen = at
}

// Remove 移除会话及其端点索引（幂等）。
func (t *UDPTable) Remove(key pairing.PairingKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[key]
	if !ok {
		return
	}
	delete(t.sessions, key)
	if s.agentReady {
		delete(t.byEndpoint, s.agent.AddrPort())
	}
	if s.clientReady {
		delete(t.byEndpoint, s.client.AddrPort())
	}
}

// Sweep 移除 lastSeen 超过 timeout 的会话，返回被移除的配对键（升序）。
//
// timeout <= 0 时不做清理。返回值供上层发送 MsgKicked 或记录日志。
func (t *UDPTable) Sweep(now time.Time, timeout time.Duration) []pairing.PairingKey {
	if timeout <= 0 {
		return nil
	}
	t.mu.Lock()
	removed := make([]pairing.PairingKey, 0, 4)
	for key, s := range t.sessions {
		if now.Sub(s.lastSeen) < timeout {
			continue
		}
		delete(t.sessions, key)
		if s.agentReady {
			delete(t.byEndpoint, s.agent.AddrPort())
		}
		if s.clientReady {
			delete(t.byEndpoint, s.client.AddrPort())
		}
		removed = append(removed, key)
	}
	t.mu.Unlock()

	sort.Slice(removed, func(i, j int) bool { return removed[i].String() < removed[j].String() })
	return removed
}

// Snapshot 返回按配对键排序的会话快照。
func (t *UDPTable) Snapshot() []UDPEntry {
	t.mu.Lock()
	out := make([]UDPEntry, 0, len(t.sessions))
	for _, s := range t.sessions {
		out = append(out, s.snapshot())
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

// Len 返回会话数量。
func (t *UDPTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}
