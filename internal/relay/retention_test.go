package relay

import (
	"errors"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/token"
)

// TestAlreadyPairedRetentionWindow 证明"一个配对键最多成功配对一次"的两层防护：
//
//  1. 配对表层：完成的条目在 pair_timeout 保留窗口内再次 Claim 返回 ErrAlreadyPaired；
//     relay 对这类报到直接关闭连接且不写任何字节（TCP 数据面不能回错误帧）。
//  2. 令牌层：保留窗口结束后条目被 Sweep 清掉，但已消费的令牌仍然拒绝重放
//     （Validate → ErrConsumed），所以"窗口过后重放"同样不可能成功。
//
// 为了可重复地命中 ErrAlreadyPaired 分支，这里直接构造"配对表已完成、令牌尚未
// Consume"的状态——它对应生产代码里 Claim 返回 nil 到 Consume 之间的极短竞态窗口。
func TestAlreadyPairedRetentionWindow(t *testing.T) {
	s, _, _, incoming, result := setupTCPPair(t, func(c *config.Config) {
		c.Relay.PairTimeout = 500 * time.Millisecond
	})

	key, err := pairing.ParsePairingKey(result.PairingKey)
	if err != nil {
		t.Fatalf("解析配对键: %v", err)
	}

	// 1) 把配对表标记为完成（模拟对端先 Claim 成功），但先不 Consume 令牌。
	if _, err := s.pairs.Claim(key, pairing.SideAgent, "placeholder-agent"); !errors.Is(err, pairing.ErrWaiting) {
		t.Fatalf("首次占位应返回 ErrWaiting，实际 %v", err)
	}
	if _, err := s.pairs.Claim(key, pairing.SideClient, "placeholder-client"); err != nil {
		t.Fatalf("另一侧占位应完成配对，实际 %v", err)
	}
	if _, err := s.pairs.Claim(key, pairing.SideAgent, "again"); !errors.Is(err, pairing.ErrAlreadyPaired) {
		t.Fatalf("完成后再 Claim 应返回 ErrAlreadyPaired，实际 %v", err)
	}
	// 令牌此时仍然有效（尚未 Consume）——这正是 already_paired 分支唯一可达的窗口。
	if err := s.tokens.Validate(key, incoming.PairingToken); err != nil {
		t.Fatalf("构造窗口失败：令牌应仍有效，实际 %v", err)
	}

	// 2) 真实数据面报到走完整 relay 管线：令牌校验通过 → Claim 返回 ErrAlreadyPaired
	//    → 连接必须被关闭，且**一个字节都不写**。
	replay := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	expectClosed(t, replay, 2*time.Second)

	// 保留窗口内条目仍在（用于拒绝重放），并且标记为 Completed。
	snaps := s.pairs.Snapshot()
	if len(snaps) != 1 || !snaps[0].Completed {
		t.Fatalf("保留窗口内应保留 1 个已完成条目，实际 %+v", snaps)
	}

	// 3) 按生产的真实顺序补上 Consume，然后等保留窗口结束 + Sweep 清理表项。
	s.tokens.Consume(key)
	waitFor(t, 3*time.Second, "完成条目在保留窗口后被清理", func() bool { return s.pairs.Len() == 0 })

	// 4) 窗口结束后重放：令牌层仍然拒绝（ErrConsumed），连接被关闭且无字节。
	afterWindow := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	expectClosed(t, afterWindow, 2*time.Second)

	// 5) 复核令牌确实处于"已消费"状态，而不是碰巧过期。
	if err := s.tokens.Validate(key, result.PairingToken); !errors.Is(err, token.ErrConsumed) {
		t.Fatalf("期望 ErrConsumed，实际 %v", err)
	}
}
