package reconnect

import (
	"context"
	"math/rand"
	"testing"
	"time"
)

// TestReconnectBackoffCap 校验指数退避序列：base ×2、cap 生效、抖动 ±20% 且不超过 cap。
func TestReconnectBackoffCap(t *testing.T) {
	const (
		base = time.Second
		max  = 8 * time.Second
	)
	b := &Backoff{Base: base, Max: max, Jitter: 0.2, Rand: rand.New(rand.NewSource(7))}

	wantRaw := func(attempt int) time.Duration {
		d := base
		for i := 1; i < attempt; i++ {
			if d >= max {
				break
			}
			d *= 2
		}
		if d > max {
			d = max
		}
		return d
	}

	for attempt := 1; attempt <= 12; attempt++ {
		raw := wantRaw(attempt)
		d := b.Delay(attempt)
		lower := time.Duration(float64(raw) * 0.8)
		upper := raw
		if raw < max {
			upper = time.Duration(float64(raw) * 1.2)
		}
		if d < lower || d > upper {
			t.Fatalf("attempt=%d delay=%s 超出 [%s, %s]（raw=%s）", attempt, d, lower, upper, raw)
		}
		if d > max {
			t.Fatalf("attempt=%d delay=%s 超过 cap=%s", attempt, d, max)
		}
	}

	// 到达 cap 后应稳定在 [0.8*max, max]。
	for attempt := 8; attempt <= 40; attempt++ {
		if d := b.Delay(attempt); d < max/5*4 || d > max {
			t.Fatalf("attempt=%d delay=%s 不在 [%s, %s]", attempt, d, max/5*4, max)
		}
	}

	// 相同种子必须给出相同序列（可测试性）。
	b1 := &Backoff{Base: base, Max: max, Jitter: 0.2, Rand: rand.New(rand.NewSource(7))}
	b2 := &Backoff{Base: base, Max: max, Jitter: 0.2, Rand: rand.New(rand.NewSource(7))}
	for attempt := 1; attempt <= 12; attempt++ {
		if a, c := b1.Delay(attempt), b2.Delay(attempt); a != c {
			t.Fatalf("同种子序列不一致：attempt=%d %s != %s", attempt, a, c)
		}
	}

	// 极大 attempt 不能溢出/为负。
	if d := b.Delay(1 << 20); d <= 0 || d > max {
		t.Fatalf("极大 attempt 的 delay=%s 非法", d)
	}
	// attempt <= 0 按第 1 次处理。
	if d := b.Delay(0); d <= 0 || d > max {
		t.Fatalf("attempt=0 的 delay=%s 非法", d)
	}
}

// TestReconnectDefaults 校验默认值：base 1s、cap 60s、±20%。
func TestReconnectDefaults(t *testing.T) {
	b := New(0)
	if b.Base != DefaultBase || b.Max != DefaultMaxBackoff {
		t.Fatalf("默认退避参数 = {base:%s max:%s}", b.Base, b.Max)
	}
	if b.Jitter != DefaultJitter {
		t.Fatalf("默认抖动 = %v", b.Jitter)
	}
	if d := b.Delay(1); d < 800*time.Millisecond || d > time.Second*6/5 {
		t.Fatalf("第 1 次 delay = %s，期望 ~1s（±20%%）", d)
	}
	// cap 生效：60s 之后不再增长（允许抖动上限 60s）。
	if d := b.Delay(20); d > DefaultMaxBackoff || d < DefaultMaxBackoff*4/5 {
		t.Fatalf("第 20 次 delay = %s，期望接近 cap=%s", d, DefaultMaxBackoff)
	}
}

// TestSleepCancelable 校验退避等待可被 ctx 取消（关停不被退避拖住）。
func TestSleepCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := Sleep(ctx, time.Hour)
	if err == nil {
		t.Fatal("ctx 已取消时 Sleep 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Sleep 未被 ctx 打断（耗时 %s）", elapsed)
	}

	// 正常等待到点返回 nil。
	if err := Sleep(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("正常 Sleep 返回 %v", err)
	}

	// Wait 返回实际等待时间。
	b := &Backoff{Base: 10 * time.Millisecond, Max: 10 * time.Millisecond, Jitter: 0}
	d, err := b.Wait(context.Background(), 3)
	if err != nil || d != 10*time.Millisecond {
		t.Fatalf("Wait = (%s, %v)，期望 (10ms, nil)", d, err)
	}
}
