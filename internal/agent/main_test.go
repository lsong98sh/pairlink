package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在整包测试跑完后用 goleak 兜底 goroutine 泄漏检查。
//
// 之前每个用例用 runtime.NumGoroutine() 记基数 + 轮询等回落（before+2 容差），
// 在 -race 全量并行负载下会被测试基础设施的短暂 goroutine 干扰而误报；goleak
// 直接按栈帧识别"测试结束后仍未退出"的 goroutine，不需要数字容差。
//
// 这里不添加任何 IgnoreTopFunction/IgnoreAnyFunction：本包测试全部在进程内
// 起停 Run 与假 relay，若 goleak 报出栈帧，那就是真实的未退出 goroutine，
// 应当定位产品代码而不是用 ignore 掩盖。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
