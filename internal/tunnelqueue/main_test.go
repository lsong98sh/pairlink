package tunnelqueue

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在整包测试跑完后用 goleak 兜底 goroutine 泄漏检查，取代原先
// runtime.NumGoroutine() 基数 + 轮询（before+2 容差）的手写判断。
//
// 本包唯一的后台 goroutine 是 tunnelqueue 的队列 writer；测试连接都在用例
// 内 Close，没有其它测试基础设施协程，因此不需要 ignore。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
