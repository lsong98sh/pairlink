package relay

import (
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// TestPairSweepIntervalFollowsPairTimeout 覆盖 P3-3：
// 扫描周期必须每次按当前 pair_timeout 重新计算，运行期把它改小后要立刻跟随，
// 不能沿用启动时的值（否则改成 200ms 仍按 5s 扫描，占位清理明显滞后）。
func TestPairSweepIntervalFollowsPairTimeout(t *testing.T) {
	s := &server{pairs: pairing.NewTable(10 * time.Second)}
	if got, want := s.pairSweepInterval(), 5*time.Second; got != want {
		t.Fatalf("初始扫描周期 = %v，期望 %v", got, want)
	}

	// 运行期改小（管理页路径）：间隔应变成 200ms/2=100ms（等于下限）。
	if err := s.pairs.SetTimeout(200 * time.Millisecond); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	if got, want := s.pairSweepInterval(), minSweepInterval; got != want {
		t.Fatalf("改小后扫描周期 = %v，期望下限 %v", got, want)
	}

	// 再改大：间隔应跟着变大。
	if err := s.pairs.SetTimeout(30 * time.Second); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	if got, want := s.pairSweepInterval(), 15*time.Second; got != want {
		t.Fatalf("改大后扫描周期 = %v，期望 %v", got, want)
	}
}
