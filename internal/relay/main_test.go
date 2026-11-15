package relay

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在整包测试跑完后用 goleak 兜底 goroutine 泄漏检查，取代原先
// runtime.NumGoroutine() 基数 + 多次采样（base+2 容差）的手写判断：容差在
// -race 或并行包负载下会误报，goleak 按栈帧精确判定未退出的 goroutine。
//
// 不加 ignore：relay.Run 的监听器、管理页、控制/数据连接都在用例内通过
// ctx 取消与 t.Cleanup 收尾，报出的任何栈帧都应是产品侧问题。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
