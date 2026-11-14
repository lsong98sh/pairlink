// Package reconnect 提供控制连接自动重连所需的指数退避与可取消等待。
//
// 退避序列（阶段 6C，docs/contract.md §2.4 第 7 条 / §2.5 第 9 条）：
//
//	delay(attempt) = min(base * 2^(attempt-1), max) * (1 ± jitter)，并不超过 max
//
// 其中 base 固定 1s，max 取 ini 的 reconnect_max_backoff（默认 60s，必须 > 0），
// jitter 为 ±20%。抖动保证多个 agent/client 在 relay 重启后不会同步重连
// （thundering herd），上限保证重连风暴不会把等待时间推到不可接受的量级。
//
// 本包不依赖任何 PairLink 内部包，便于 agent / client 共用同一套语义。
package reconnect

import (
	"context"
	"math/rand"
	"time"
)

const (
	// DefaultBase 是首次重连的基准等待时间（不可配置，避免退避过密）。
	DefaultBase = time.Second
	// DefaultMaxBackoff 是退避上限的默认值（[server]/[client] reconnect_max_backoff）。
	DefaultMaxBackoff = 60 * time.Second
	// DefaultJitter 是抖动比例（±20%）。
	DefaultJitter = 0.2
	// maxShift 限制指数增长的最大位移，避免整型溢出。
	maxShift = 32
)

// Backoff 是一条带抖动的指数退避序列。
//
// 并发约定：Backoff 只被「一条控制连接的监督循环」使用；Rand 不与其它 goroutine 共享。
type Backoff struct {
	// Base 是首次重试的基准等待时间；<= 0 时使用 DefaultBase。
	Base time.Duration
	// Max 是退避上限；<= 0 时使用 DefaultMaxBackoff。
	Max time.Duration
	// Jitter 是抖动比例（0.2 表示 ±20%）；< 0 时使用 DefaultJitter，0 表示不抖动。
	Jitter float64
	// Rand 是抖动用的随机源；nil 时使用以当前时间播种的独立随机源。
	Rand *rand.Rand
}

// New 构造一条退避序列（base 固定 DefaultBase，max 由配置给出）。
func New(max time.Duration) *Backoff {
	if max <= 0 {
		max = DefaultMaxBackoff
	}
	return &Backoff{
		Base:   DefaultBase,
		Max:    max,
		Jitter: DefaultJitter,
		Rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Delay 返回第 attempt 次重连（attempt 从 1 开始）的等待时间。
//
// 结果始终 <= Max，且 >= 0.8*min(Base*2^(attempt-1), Max)（抖动下限）。
func (b *Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := b.Base
	if base <= 0 {
		base = DefaultBase
	}
	max := b.Max
	if max <= 0 {
		max = DefaultMaxBackoff
	}
	if base > max {
		base = max
	}

	raw := base
	for i := 1; i < attempt && i <= maxShift; i++ {
		if raw >= max {
			break
		}
		raw *= 2
	}
	if raw > max || raw <= 0 {
		raw = max
	}

	jitter := b.Jitter
	if jitter < 0 {
		jitter = DefaultJitter
	}
	if jitter == 0 {
		return raw
	}
	if jitter > 1 {
		jitter = 1
	}
	r := b.rand()
	// r ∈ [1-jitter, 1+jitter]。
	factor := 1 - jitter + 2*jitter*r
	d := time.Duration(float64(raw) * factor)
	if d > max {
		d = max
	}
	if d < 0 {
		d = 0
	}
	return d
}

// rand 返回 [0,1) 的随机数。
func (b *Backoff) rand() float64 {
	if b.Rand == nil {
		b.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return b.Rand.Float64()
}

// Sleep 等待 d，期间 ctx 取消立即返回 ctx.Err()。
//
// 用于退避等待：必须可被 ctx 打断，否则关停要等到退避结束。
func Sleep(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Wait 计算第 attempt 次退避并等待；ctx 取消返回 ctx.Err()。
func (b *Backoff) Wait(ctx context.Context, attempt int) (time.Duration, error) {
	d := b.Delay(attempt)
	return d, Sleep(ctx, d)
}
