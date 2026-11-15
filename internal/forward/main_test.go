package forward

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在整包测试跑完后用 goleak 兜底 goroutine 泄漏检查，取代原先
// runtime.NumGoroutine() 基数 + 轮询（baseline+2 容差）的手写判断。
//
// 本包没有常驻测试基础设施协程：net.Pipe/TCP 连接都在用例内关闭，
// goleak 报出的任何栈帧都指向 forward 自己的 copy/看护 goroutine 未退出。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
