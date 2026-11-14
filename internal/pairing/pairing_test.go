package pairing

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func testKey(service, conn string) PairingKey {
	return PairingKey{ServiceID: service, ClientConnID: conn}
}

func TestPairingKey(t *testing.T) {
	k := testKey("web", "c-1")
	if got, want := k.String(), "web/c-1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	parsed, err := ParsePairingKey("web/c-1")
	if err != nil {
		t.Fatalf("ParsePairingKey 失败: %v", err)
	}
	if parsed != k {
		t.Errorf("ParsePairingKey = %+v, want %+v", parsed, k)
	}
	if err := k.Validate(); err != nil {
		t.Errorf("合法配对键校验失败: %v", err)
	}
	if !(PairingKey{}).IsZero() {
		t.Error("零值配对键 IsZero 应为 true")
	}

	bad := []string{"", "web", "/c-1", "web/", "web/c 1", "WEB/c-1", "web/c-1/extra"}
	for _, s := range bad {
		if _, err := ParsePairingKey(s); err == nil {
			t.Errorf("ParsePairingKey(%q) 应报错", s)
		}
	}
}

func TestSideString(t *testing.T) {
	if SideAgent.String() != "agent" || SideClient.String() != "client" || SideUnknown.String() != "unknown" {
		t.Error("Side.String 错误")
	}
	if !SideAgent.Valid() || SideUnknown.Valid() {
		t.Error("Side.Valid 错误")
	}
}

func TestClaimBothOrders(t *testing.T) {
	orders := []struct {
		name string
		side Side
	}{
		{"agent 先到", SideAgent},
		{"client 先到", SideClient},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			tbl := NewTable(5 * time.Second)
			key := testKey("web", "c-1")
			agentPeer := "agent-conn"
			clientPeer := "client-conn"

			first, second := tc.side, SideClient
			if tc.side == SideClient {
				second = SideAgent
			}
			firstPeer, secondPeer := any(clientPeer), any(agentPeer)
			if tc.side == SideAgent {
				firstPeer, secondPeer = agentPeer, clientPeer
			}

			if _, err := tbl.Claim(key, first, firstPeer); !errors.Is(err, ErrWaiting) {
				t.Fatalf("首次报到应返回 ErrWaiting，实际: %v", err)
			}
			pair, err := tbl.Claim(key, second, secondPeer)
			if err != nil {
				t.Fatalf("另一侧到达应成功，实际: %v", err)
			}
			if pair.Key != key || pair.Agent != agentPeer || pair.Client != clientPeer {
				t.Fatalf("配对结果错误: %+v", pair)
			}
			if _, err := tbl.Claim(key, second, secondPeer); !errors.Is(err, ErrAlreadyPaired) {
				t.Fatalf("完成后再次报到应返回 ErrAlreadyPaired，实际: %v", err)
			}
			snap := tbl.Snapshot()
			if len(snap) != 1 || !snap[0].Completed || !snap[0].AgentArrived || !snap[0].ClientArrived {
				t.Fatalf("快照错误: %+v", snap)
			}
		})
	}
}

func TestClaimDuplicateSide(t *testing.T) {
	tbl := NewTable(time.Second)
	key := testKey("web", "c-2")
	if _, err := tbl.Claim(key, SideAgent, "a1"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("首次报到失败: %v", err)
	}
	if _, err := tbl.Claim(key, SideAgent, "a2"); !errors.Is(err, ErrDuplicateSide) {
		t.Fatalf("同侧重复报到应返回 ErrDuplicateSide，实际: %v", err)
	}
	if tbl.Len() != 1 {
		t.Fatalf("重复报到不应新增条目: %d", tbl.Len())
	}
}

func TestClaimInvalidSide(t *testing.T) {
	tbl := NewTable(time.Second)
	if _, err := tbl.Claim(testKey("web", "c"), SideUnknown, nil); !errors.Is(err, ErrInvalidSide) {
		t.Fatalf("非法侧别应返回 ErrInvalidSide，实际: %v", err)
	}
}

func TestClaimTimeout(t *testing.T) {
	clock := newFakeClock()
	tbl := NewTable(10 * time.Second)
	tbl.SetClock(clock.Now)

	var expiredKey PairingKey
	var expiredSide Side
	var expiredPeer any
	tbl.OnExpire = func(key PairingKey, side Side, peer any) {
		expiredKey, expiredSide, expiredPeer = key, side, peer
	}

	key := testKey("web", "c-3")
	if _, err := tbl.Claim(key, SideAgent, "agent-conn"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("首次报到失败: %v", err)
	}
	clock.Advance(11 * time.Second)
	if _, err := tbl.Claim(key, SideClient, "client-conn"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("超时后报到应返回 ErrTimeout，实际: %v", err)
	}
	if expiredKey != key || expiredSide != SideAgent || expiredPeer != "agent-conn" {
		t.Fatalf("超时回调参数错误: key=%v side=%v peer=%v", expiredKey, expiredSide, expiredPeer)
	}
	if tbl.Len() != 0 {
		t.Fatalf("超时占位应被移除，剩余 %d", tbl.Len())
	}
}

func TestSweep(t *testing.T) {
	clock := newFakeClock()
	tbl := NewTable(5 * time.Second)
	tbl.SetClock(clock.Now)

	var calls int32
	tbl.OnExpire = func(PairingKey, Side, any) { atomic.AddInt32(&calls, 1) }

	if _, err := tbl.Claim(testKey("web", "a"), SideAgent, "a"); err != nil && !errors.Is(err, ErrWaiting) {
		t.Fatalf("报到失败: %v", err)
	}
	if n := tbl.Sweep(clock.Now()); n != 0 {
		t.Fatalf("未超时时 Sweep 应返回 0，实际 %d", n)
	}
	clock.Advance(6 * time.Second)
	if n := tbl.Sweep(clock.Now()); n != 1 {
		t.Fatalf("超时后 Sweep 应返回 1，实际 %d", n)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("OnExpire 调用次数 = %d, want 1", got)
	}
	if n := tbl.Sweep(clock.Now()); n != 0 {
		t.Fatalf("重复 Sweep 应返回 0（幂等），实际 %d", n)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("重复 Sweep 不应再次回调 OnExpire，实际 %d", got)
	}
}

func TestSweepCompletedNoExpire(t *testing.T) {
	clock := newFakeClock()
	tbl := NewTable(5 * time.Second)
	tbl.SetClock(clock.Now)

	var calls int32
	tbl.OnExpire = func(PairingKey, Side, any) { atomic.AddInt32(&calls, 1) }

	key := testKey("web", "b")
	if _, err := tbl.Claim(key, SideAgent, "a"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("报到失败: %v", err)
	}
	if _, err := tbl.Claim(key, SideClient, "c"); err != nil {
		t.Fatalf("配对失败: %v", err)
	}
	clock.Advance(6 * time.Second)
	if n := tbl.Sweep(clock.Now()); n != 1 {
		t.Fatalf("完成条目到期应被清理，Sweep = %d", n)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("已完成条目不应触发 OnExpire，实际 %d", got)
	}
}

func TestRemoveIdempotent(t *testing.T) {
	tbl := NewTable(time.Second)
	key := testKey("web", "c-4")
	if _, err := tbl.Claim(key, SideClient, "c"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("报到失败: %v", err)
	}
	tbl.Remove(key)
	tbl.Remove(key)
	if tbl.Len() != 0 {
		t.Fatalf("Remove 后应为空，实际 %d", tbl.Len())
	}
	// 移除后可重新使用同一个键。
	if _, err := tbl.Claim(key, SideClient, "c2"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("Remove 后重新报到失败: %v", err)
	}
}

func TestSnapshotRemaining(t *testing.T) {
	clock := newFakeClock()
	tbl := NewTable(10 * time.Second)
	tbl.SetClock(clock.Now)

	if _, err := tbl.Claim(testKey("b", "x"), SideAgent, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("报到失败: %v", err)
	}
	if _, err := tbl.Claim(testKey("a", "x"), SideClient, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("报到失败: %v", err)
	}
	clock.Advance(4 * time.Second)
	snap := tbl.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("快照长度 = %d, want 2", len(snap))
	}
	if snap[0].Key.String() != "a/x" || snap[1].Key.String() != "b/x" {
		t.Fatalf("快照应按 key 排序: %+v", snap)
	}
	if snap[0].Remaining != 6*time.Second {
		t.Fatalf("Remaining = %v, want 6s", snap[0].Remaining)
	}
	if snap[1].AgentArrived != true || snap[1].ClientArrived != false {
		t.Fatalf("快照到达状态错误: %+v", snap[1])
	}
	if tbl.Timeout() != 10*time.Second {
		t.Fatalf("Timeout() = %v", tbl.Timeout())
	}
}

func TestConcurrentPairings(t *testing.T) {
	tbl := NewTable(5 * time.Second)
	const n = 100

	var wg sync.WaitGroup
	results := make([]Pair, n)
	errs := make(chan error, 2*n)

	for i := 0; i < n; i++ {
		key := testKey("svc", "conn-"+itoa(i))
		peers := [2]any{"agent-" + itoa(i), "client-" + itoa(i)}

		// 两侧并发到达，顺序随机（由调度决定）。
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			pair, err := tbl.Claim(key, SideAgent, peers[0])
			if err != nil {
				if !errors.Is(err, ErrWaiting) {
					errs <- err
				}
				return
			}
			results[idx] = pair
		}(i)
		go func(idx int) {
			defer wg.Done()
			pair, err := tbl.Claim(key, SideClient, peers[1])
			if err != nil {
				if !errors.Is(err, ErrWaiting) {
					errs <- err
				}
				return
			}
			results[idx] = pair
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发配对出现错误: %v", err)
	}

	paired := 0
	for i, pair := range results {
		if pair.Key.ServiceID == "" {
			t.Fatalf("配对 %d 未完成", i)
		}
		if pair.Agent == nil || pair.Client == nil {
			t.Fatalf("配对 %d 缺失 peer: %+v", i, pair)
		}
		if pair.Key != testKey("svc", "conn-"+itoa(i)) {
			t.Fatalf("配对 %d 键错误: %v", i, pair.Key)
		}
		paired++
	}
	if paired != n {
		t.Fatalf("完成配对数 = %d, want %d", paired, n)
	}
	if tbl.Len() != n {
		t.Fatalf("完成后条目数 = %d, want %d", tbl.Len(), n)
	}
}

// itoa 是 strconv.Itoa 的轻量替代，避免测试文件引入额外依赖。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
