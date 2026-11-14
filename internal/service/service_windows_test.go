//go:build windows

package service

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/sys/windows/svc"
)

// 编译期签名检查：阶段2 的 main.go 依赖这些确切签名。
var (
	_ func(name, displayName string, args []string) error          = Install
	_ func(name string) error                                      = Uninstall
	_ func(name string) error                                      = Start
	_ func(name string) error                                      = Stop
	_ func(name string) (Status, error)                            = Query
	_ func(name string, run func(ctx context.Context) error) error = Run
	_ func() bool                                                  = IsWindowsService
)

func TestManagerAPIShape(t *testing.T) {
	// 状态映射。
	tests := map[svc.State]State{
		svc.Stopped:      StateStopped,
		svc.StartPending: StateStartPending,
		svc.StopPending:  StateStopPending,
		svc.Running:      StateRunning,
		svc.Paused:       StatePaused,
	}
	for in, want := range tests {
		if got := fromWindowsState(in); got != want {
			t.Errorf("fromWindowsState(%v) = %v, want %v", in, got, want)
		}
	}
	if IsWindowsService() {
		t.Log("当前进程由 SCM 启动")
	}
}

// TestQueryMissingService 只做只读查询，不安装任何服务。
//
// 非管理员账号可能没有连接 SCM 的权限，此时跳过而不是失败。
func TestQueryMissingService(t *testing.T) {
	name := "pairlink-does-not-exist-test"
	st, err := Query(name)
	if err == nil {
		t.Fatalf("查询不存在的服务应报错，实际返回 %+v", st)
	}
	if strings.Contains(strings.ToLower(err.Error()), "access is denied") ||
		strings.Contains(err.Error(), "拒绝访问") {
		t.Skipf("无 SCM 访问权限，跳过: %v", err)
	}
	t.Logf("查询不存在的服务按预期失败: %v", err)
}
