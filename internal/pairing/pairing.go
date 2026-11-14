// Package pairing 定义配对键与待配对表。
//
// 配对键 = (服务ID, 客户端连接ID)，字符串形式为 "serviceID/clientConnID"。
// 待配对表负责处理 agent 与 client 两侧到达顺序不确定的情况：
// 先到者占位，另一侧到达后完成配对，超时由 Sweep 统一清理（不启动额外 goroutine）。
package pairing

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// idPattern 限定服务ID / 客户端连接ID 的字符与长度（1..64）。
var idPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// 待配对表相关哨兵错误，可用 errors.Is 判别。
var (
	// ErrWaiting 表示该配对键只有一侧到达，正在等待对端。
	ErrWaiting = errors.New("pairing: 等待对端报到")
	// ErrTimeout 表示该配对键的占位已超时（超时项已被移除）。
	ErrTimeout = errors.New("pairing: 配对超时")
	// ErrDuplicateSide 表示同一配对键的同一侧重复报到。
	ErrDuplicateSide = errors.New("pairing: 同一侧重复报到")
	// ErrAlreadyPaired 表示该配对键已完成配对，再次报到被拒绝。
	ErrAlreadyPaired = errors.New("pairing: 配对已完成")
	// ErrInvalidSide 表示非法的侧别。
	ErrInvalidSide = errors.New("pairing: 非法的侧别")
)

// PairingKey 是配对键，标识一次端到端配对会话。
type PairingKey struct {
	// ServiceID 是服务ID。
	ServiceID string
	// ClientConnID 是 client 为本次本地连接生成的连接ID。
	ClientConnID string
}

// String 返回 "serviceID/clientConnID"。
func (k PairingKey) String() string {
	return k.ServiceID + "/" + k.ClientConnID
}

// IsZero 报告配对键是否为零值。
func (k PairingKey) IsZero() bool {
	return k.ServiceID == "" && k.ClientConnID == ""
}

// Validate 校验配对键的组成：两段均非空、符合字符集与长度限制。
func (k PairingKey) Validate() error {
	if k.ServiceID == "" {
		return fmt.Errorf("pairing: 配对键缺少服务ID")
	}
	if k.ClientConnID == "" {
		return fmt.Errorf("pairing: 配对键缺少客户端连接ID")
	}
	if !idPattern.MatchString(k.ServiceID) {
		return fmt.Errorf("pairing: 服务ID %q 非法，只允许 a-z0-9-_ 且长度 1..64", k.ServiceID)
	}
	if !idPattern.MatchString(k.ClientConnID) {
		return fmt.Errorf("pairing: 客户端连接ID %q 非法，只允许 a-z0-9-_ 且长度 1..64", k.ClientConnID)
	}
	return nil
}

// ParsePairingKey 解析 "serviceID/clientConnID" 形式的配对键。
func ParsePairingKey(s string) (PairingKey, error) {
	serviceID, clientConnID, ok := strings.Cut(s, "/")
	if !ok {
		return PairingKey{}, fmt.Errorf("pairing: 配对键 %q 缺少 '/' 分隔符", s)
	}
	key := PairingKey{ServiceID: serviceID, ClientConnID: clientConnID}
	if err := key.Validate(); err != nil {
		return PairingKey{}, err
	}
	return key, nil
}

// Side 表示报到方。
type Side int

const (
	// SideUnknown 是零值，表示未指定侧别。
	SideUnknown Side = iota
	// SideAgent 表示服务提供者侧。
	SideAgent
	// SideClient 表示访问侧。
	SideClient
)

// Valid 报告 s 是否是有效侧别。
func (s Side) Valid() bool { return s == SideAgent || s == SideClient }

// String 实现 fmt.Stringer。
func (s Side) String() string {
	switch s {
	case SideAgent:
		return "agent"
	case SideClient:
		return "client"
	default:
		return "unknown"
	}
}

// Pair 是配对完成的结果，Agent / Client 为调用方传入的 peer 对象
// （通常是数据面连接或 UDP 端点）。
type Pair struct {
	// Key 是配对键。
	Key PairingKey
	// Agent 是 agent 侧 peer。
	Agent any
	// Client 是 client 侧 peer。
	Client any
}

// PairingSnapshot 是待配对表的一行快照，供管理页展示。
type PairingSnapshot struct {
	// Key 是配对键。
	Key PairingKey
	// AgentArrived 表示 agent 侧已报到。
	AgentArrived bool
	// ClientArrived 表示 client 侧已报到。
	ClientArrived bool
	// Completed 表示配对已完成（等待清理窗口结束）。
	Completed bool
	// CreatedAt 是首次报到时间。
	CreatedAt time.Time
	// Deadline 是超时截止时间。
	Deadline time.Time
	// Remaining 是距离截止的剩余时间（不小于 0）。
	Remaining time.Duration
}

// entry 是待配对表中的一个占位。
type entry struct {
	key       PairingKey
	agent     any
	client    any
	hasAgent  bool
	hasClient bool
	createdAt time.Time
	deadline  time.Time
	completed bool
}

// PairingTable 是并发安全的待配对表。
//
// 并发保护使用单把 sync.Mutex；超时清理完全依赖调用方以单一 ticker 调用
// Sweep，不使用 time.After，因此不会产生额外 goroutine。
type PairingTable struct {
	mu      sync.Mutex
	timeout time.Duration
	now     func() time.Time
	entries map[PairingKey]*entry
	// OnExpire 是超时清理回调，用于关闭超时占位的连接。
	//
	// 回调在锁外调用（Sweep / Claim 内清理时触发），实现中可以安全地操作
	// PairingTable 或关闭连接；回调必须快速返回。应在开始服务前设置。
	OnExpire func(key PairingKey, side Side, peer any)
}

// SetOnExpire 设置超时清理回调（与直接写 OnExpire 等价，额外提供加锁版本）。
func (t *PairingTable) SetOnExpire(fn func(key PairingKey, side Side, peer any)) {
	t.mu.Lock()
	t.OnExpire = fn
	t.mu.Unlock()
}

// NewTable 创建一个待配对表，pairTimeout <= 0 时按 10s 处理。
func NewTable(pairTimeout time.Duration) *PairingTable {
	if pairTimeout <= 0 {
		pairTimeout = 10 * time.Second
	}
	return &PairingTable{
		timeout: pairTimeout,
		now:     time.Now,
		entries: make(map[PairingKey]*entry),
	}
}

// Timeout 返回占位超时时间。
func (t *PairingTable) Timeout() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timeout
}

// Claim 登记一侧报到。
//
// 返回值语义：
//   - 首次报到：返回 (Pair{}, ErrWaiting)，已占位等待对端；
//   - 另一侧到达：返回 (Pair{Key, Agent, Client}, nil)，配对完成；
//   - 同侧重复报到：返回 ErrDuplicateSide；
//   - 占位已超时：返回 ErrTimeout（占位已被移除，并通过 OnExpire 通知）；
//   - 该键已完成配对：返回 ErrAlreadyPaired。
func (t *PairingTable) Claim(key PairingKey, side Side, peer any) (Pair, error) {
	if !side.Valid() {
		return Pair{}, ErrInvalidSide
	}
	now := t.now()

	var expired *entry
	var result Pair
	var err error

	t.mu.Lock()
	if t.entries == nil {
		t.entries = make(map[PairingKey]*entry)
	}
	e, ok := t.entries[key]
	switch {
	case !ok:
		t.entries[key] = &entry{
			key:       key,
			agent:     agentPeer(side, peer),
			client:    clientPeer(side, peer),
			hasAgent:  side == SideAgent,
			hasClient: side == SideClient,
			createdAt: now,
			deadline:  now.Add(t.timeout),
		}
		err = ErrWaiting
	case e.completed:
		err = ErrAlreadyPaired
	case !now.Before(e.deadline):
		delete(t.entries, key)
		expired = e
		err = ErrTimeout
	case (side == SideAgent && e.hasAgent) || (side == SideClient && e.hasClient):
		err = ErrDuplicateSide
	default:
		if side == SideAgent {
			e.agent, e.hasAgent = peer, true
		} else {
			e.client, e.hasClient = peer, true
		}
		e.completed = true
		// 完成后再保留一个超时窗口，用于拒绝重放并等待调用方 Remove。
		e.deadline = now.Add(t.timeout)
		result = Pair{Key: key, Agent: e.agent, Client: e.client}
	}
	t.mu.Unlock()

	if expired != nil {
		t.fireExpire(expired)
	}
	return result, err
}

// agentPeer 在 side 为 agent 时返回 peer，否则返回 nil。
func agentPeer(side Side, peer any) any {
	if side == SideAgent {
		return peer
	}
	return nil
}

// clientPeer 在 side 为 client 时返回 peer，否则返回 nil。
func clientPeer(side Side, peer any) any {
	if side == SideClient {
		return peer
	}
	return nil
}

// fireExpire 在锁外执行超时回调。
func (t *PairingTable) fireExpire(e *entry) {
	t.mu.Lock()
	fn := t.OnExpire
	t.mu.Unlock()
	if fn == nil {
		return
	}
	if e.hasAgent {
		fn(e.key, SideAgent, e.agent)
	}
	if e.hasClient {
		fn(e.key, SideClient, e.client)
	}
}

// Remove 移除指定配对键的占位（幂等），通常在配对结束/连接关闭时调用。
func (t *PairingTable) Remove(key PairingKey) {
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
}

// Sweep 关闭并移除所有到期占位，返回移除数量。
//
// 未完成的占位会触发 OnExpire；已完成的占位只做清理，不再触发回调。
// 应由单一 ticker 驱动（例如 pair_timeout/2 一次），避免重复清理。
func (t *PairingTable) Sweep(now time.Time) int {
	var expired []*entry
	removed := 0

	t.mu.Lock()
	for key, e := range t.entries {
		if now.Before(e.deadline) {
			continue
		}
		delete(t.entries, key)
		removed++
		if !e.completed {
			expired = append(expired, e)
		}
	}
	t.mu.Unlock()

	for _, e := range expired {
		t.fireExpire(e)
	}
	return removed
}

// Snapshot 返回按配对键排序的当前占位快照。
func (t *PairingTable) Snapshot() []PairingSnapshot {
	now := t.now()

	t.mu.Lock()
	out := make([]PairingSnapshot, 0, len(t.entries))
	for _, e := range t.entries {
		remaining := e.deadline.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, PairingSnapshot{
			Key:           e.key,
			AgentArrived:  e.hasAgent,
			ClientArrived: e.hasClient,
			Completed:     e.completed,
			CreatedAt:     e.createdAt,
			Deadline:      e.deadline,
			Remaining:     remaining,
		})
	}
	t.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].Key.String() < out[j].Key.String()
	})
	return out
}

// Len 返回当前占位数量（含已配对但仍在保留窗口内的条目）。
func (t *PairingTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// SetClock 替换时间源，仅用于测试。传入 nil 恢复 time.Now。
func (t *PairingTable) SetClock(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	t.now = now
}
