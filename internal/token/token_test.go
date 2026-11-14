package token

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// fakeClock 是可手动推进的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1700000000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func key(service, conn string) pairing.PairingKey {
	return pairing.PairingKey{ServiceID: service, ClientConnID: conn}
}

func TestIssueAndValidate(t *testing.T) {
	s := NewStore(15 * time.Second)
	k := key("web", "c-1")

	tok, err := s.Issue(k)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	if len(tok) != 43 {
		t.Fatalf("令牌长度 = %d, want 43（32 字节 base64url 无填充）: %q", len(tok), tok)
	}
	if err := s.Validate(k, tok); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	if s.TTL() != 15*time.Second {
		t.Fatalf("TTL = %v", s.TTL())
	}

	// 同一配对键重新签发：新令牌有效，旧令牌失效。
	tok2, err := s.Issue(k)
	if err != nil {
		t.Fatalf("再次 Issue 失败: %v", err)
	}
	if tok2 == tok {
		t.Fatal("两次签发的令牌不应相同")
	}
	if err := s.Validate(k, tok2); err != nil {
		t.Fatalf("新令牌应有效: %v", err)
	}
	if err := s.Validate(k, tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("旧令牌应失效，实际: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("同一配对键只应有一个条目，实际 %d", s.Len())
	}
}

func TestValidateMismatch(t *testing.T) {
	s := NewStore(time.Minute)
	k := key("web", "c-1")
	tok, err := s.Issue(k)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	if err := s.Validate(key("web", "c-2"), tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("不同配对键应返回 ErrInvalid，实际: %v", err)
	}
	if err := s.Validate(k, tok+"x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("错误令牌应返回 ErrInvalid，实际: %v", err)
	}
	if err := s.Validate(k, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空令牌应返回 ErrInvalid，实际: %v", err)
	}
	if err := s.Validate(key("none", "none"), tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("不存在的配对键应返回 ErrInvalid，实际: %v", err)
	}
}

func TestConsume(t *testing.T) {
	s := NewStore(time.Minute)
	k := key("web", "c-1")
	tok, err := s.Issue(k)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	s.Consume(k)
	s.Consume(k)
	if err := s.Validate(k, tok); !errors.Is(err, ErrConsumed) {
		t.Fatalf("消费后应返回 ErrConsumed，实际: %v", err)
	}
	if err := s.Validate(k, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("消费后错误令牌仍应返回 ErrInvalid，实际: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	clock := newFakeClock()
	s := NewStore(10 * time.Second)
	s.SetClock(clock.Now)
	k := key("web", "c-1")
	tok, err := s.Issue(k)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	clock.Advance(9 * time.Second)
	if err := s.Validate(k, tok); err != nil {
		t.Fatalf("未过期时应有效: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := s.Validate(k, tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期后应返回 ErrExpired，实际: %v", err)
	}
	if n := s.Sweep(); n != 1 {
		t.Fatalf("Sweep 应清理 1 个过期条目，实际 %d", n)
	}
	if s.Len() != 0 {
		t.Fatalf("Sweep 后 Len = %d, want 0", s.Len())
	}
	if n := s.Sweep(); n != 0 {
		t.Fatalf("重复 Sweep 应返回 0，实际 %d", n)
	}
}

func TestSweepKeepsValid(t *testing.T) {
	clock := newFakeClock()
	s := NewStore(10 * time.Second)
	s.SetClock(clock.Now)

	oldKey, newKey := key("a", "1"), key("b", "2")
	oldTok, err := s.Issue(oldKey)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	clock.Advance(6 * time.Second)
	newTok, err := s.Issue(newKey)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	clock.Advance(5 * time.Second)

	if n := s.Sweep(); n != 1 {
		t.Fatalf("Sweep = %d, want 1", n)
	}
	if err := s.Validate(newKey, newTok); err != nil {
		t.Fatalf("未过期令牌不应被清理: %v", err)
	}
	if err := s.Validate(oldKey, oldTok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("已清理令牌应返回 ErrInvalid，实际: %v", err)
	}
}

func TestDefaultTTL(t *testing.T) {
	if got := NewStore(0).TTL(); got != defaultTTL {
		t.Fatalf("ttl<=0 应使用默认 %v，实际 %v", defaultTTL, got)
	}
	if got := NewStore(-time.Second).TTL(); got != defaultTTL {
		t.Fatalf("ttl<0 应使用默认 %v，实际 %v", defaultTTL, got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStore(time.Minute)
	const workers = 32
	const perWorker = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				k := key("svc", fmt.Sprintf("c-%d-%d", w, i))
				tok, err := s.Issue(k)
				if err != nil {
					t.Errorf("Issue 失败: %v", err)
					return
				}
				if err := s.Validate(k, tok); err != nil {
					t.Errorf("Validate 失败: %v", err)
					return
				}
				s.Consume(k)
				if err := s.Validate(k, tok); !errors.Is(err, ErrConsumed) {
					t.Errorf("消费后应返回 ErrConsumed，实际: %v", err)
					return
				}
				if i%7 == 0 {
					s.Sweep()
					_ = s.Len()
				}
			}
		}(w)
	}
	wg.Wait()
	if s.Len() != workers*perWorker {
		t.Fatalf("Len = %d, want %d", s.Len(), workers*perWorker)
	}
}
