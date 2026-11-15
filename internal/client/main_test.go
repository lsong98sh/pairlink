package client

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在整包测试跑完后用 goleak 兜底 goroutine 泄漏检查，取代原先
// runtime.NumGoroutine() 基数 + 轮询（base+2 容差）的手写判断：
// 数字容差会在 -race 等并行负载下误报，goleak 按栈帧判定，不需要容差。
//
// 本包没有需要 ignore 的测试基础设施 goroutine：假 relay、监听器、Run
// 都由各用例的 t.Cleanup 收尾，测试进程内也没有常驻后台协程。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
