// Package token 实现一次性配对令牌（PairingToken）。
//
// 令牌由 relay 在信令阶段签发，与配对键绑定，短有效期，用后即焚；
// TCP 与 UDP 数据面报到时都必须校验。存储内只保存令牌的 SHA-256，
// 不保存明文；比较使用 subtle.ConstantTimeCompare 防时序侧信道。
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// 令牌校验相关哨兵错误，可用 errors.Is 判别。
var (
	// ErrInvalid 表示令牌不存在或与配对键不匹配。
	ErrInvalid = errors.New("token: 配对令牌无效")
	// ErrExpired 表示令牌已过期。
	ErrExpired = errors.New("token: 配对令牌已过期")
	// ErrConsumed 表示令牌已被消费（一次性）。
	ErrConsumed = errors.New("token: 配对令牌已被使用")
)

// defaultTTL 是 ttl <= 0 时的兜底有效期。
const defaultTTL = 15 * time.Second

// entry 是一个令牌条目，只保存摘要。
type entry struct {
	hash      [sha256.Size]byte
	issuedAt  time.Time
	expiresAt time.Time
	consumed  bool
}

// Store 是并发安全的一次性令牌表，以配对键为索引。
type Store struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]*entry
}

// PairingToken 是 Store 的别名，对应 spec 中的命名约定。
type PairingToken = Store

// NewStore 创建令牌表，ttl 为令牌有效期；ttl <= 0 时按 15s 处理。
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Store{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*entry),
	}
}

// TTL 返回令牌有效期。
func (s *Store) TTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// Issue 为配对键签发一个新令牌（32 字节 crypto/rand → base64url 无填充）。
//
// 同一配对键重复签发会覆盖旧令牌，旧令牌立即失效。
func (s *Store) Issue(key pairing.PairingKey) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token: 生成随机令牌: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(tok))

	now := s.nowFor()
	s.mu.Lock()
	s.entries[key.String()] = &entry{
		hash:      hash,
		issuedAt:  now,
		expiresAt: now.Add(s.ttl),
	}
	s.mu.Unlock()
	return tok, nil
}

// Validate 校验令牌：存在、与配对键绑定、未过期、未消费。
//
// 四个条件中任一不满足都返回对应哨兵错误；令牌摘要比较使用常量时间。
func (s *Store) Validate(key pairing.PairingKey, tok string) error {
	id := key.String()
	hash := sha256.Sum256([]byte(tok))

	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return ErrInvalid
	}
	match := subtle.ConstantTimeCompare(hash[:], e.hash[:]) == 1
	expired := !s.nowLocked().Before(e.expiresAt)
	consumed := e.consumed
	s.mu.Unlock()

	switch {
	case !match:
		return ErrInvalid
	case expired:
		return ErrExpired
	case consumed:
		return ErrConsumed
	default:
		return nil
	}
}

// Consume 标记配对键对应的令牌已使用（幂等）。
//
// 配对成功后调用；此后 Validate 返回 ErrConsumed。条目会保留到过期，
// 以便区分"已使用"与"不存在"。
func (s *Store) Consume(key pairing.PairingKey) {
	id := key.String()
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.consumed = true
	}
	s.mu.Unlock()
}

// Sweep 清理已过期的令牌，返回清理数量。
//
// 应由单一 ticker 驱动（例如每秒一次）；已消费但未过期的条目保留到过期。
func (s *Store) Sweep() int {
	now := s.nowFor()
	removed := 0
	s.mu.Lock()
	for id, e := range s.entries {
		if !now.Before(e.expiresAt) {
			delete(s.entries, id)
			removed++
		}
	}
	s.mu.Unlock()
	return removed
}

// Len 返回当前条目数（含已消费、尚未过期的条目）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// nowFor 读取时间源（加锁，允许测试替换）。
func (s *Store) nowFor() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	return now()
}

// nowLocked 在已持有 s.mu 时读取当前时间。
func (s *Store) nowLocked() time.Time {
	return s.now()
}

// SetClock 替换时间源，仅用于测试。传入 nil 恢复 time.Now。
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	s.now = now
}
