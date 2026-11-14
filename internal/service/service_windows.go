//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// serviceStopTimeout 是停止服务时等待 SCM 状态变化的上限。
	serviceStopTimeout = 15 * time.Second
	// serviceStartTimeout 是启动服务时等待 SCM 状态变化的上限。
	serviceStartTimeout = 15 * time.Second
	// runExitTimeout 是收到 Stop 控制后等待业务退出的上限。
	runExitTimeout = 10 * time.Second
)

// Install 安装 Windows 服务。
//
// 可执行文件路径取 os.Executable()，启动类型为自动（StartAutomatic），
// args 是服务启动时传给进程的命令行参数。调用方应传入 -config 的绝对路径，
// 因为 SCM 启动服务时的工作目录不是 exe 所在目录。
func Install(name, displayName string, args []string) error {
	if name == "" {
		return fmt.Errorf("service: 服务名不能为空")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service: 获取可执行文件路径: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("service: 解析可执行文件路径: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: 连接 SCM（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(name); err == nil {
		existing.Close()
		return fmt.Errorf("service: 服务 %s 已存在，请先 uninstall", name)
	}

	if displayName == "" {
		displayName = name
	}
	s, err := m.CreateService(name, exe, mgr.Config{
		DisplayName: displayName,
		Description: "PairLink 配对式中继系统（" + name + "）",
		StartType:   mgr.StartAutomatic,
	}, args...)
	if err != nil {
		return fmt.Errorf("service: 创建服务 %s: %w", name, err)
	}
	defer s.Close()
	return nil
}

// Uninstall 停止并删除 Windows 服务。
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: 连接 SCM（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service: 打开服务 %s: %w", name, err)
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("service: 停止服务 %s: %w", name, err)
		}
		if err := waitState(s, svc.Stopped, serviceStopTimeout); err != nil {
			return err
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("service: 删除服务 %s: %w", name, err)
	}
	return nil
}

// Start 启动服务并等待其进入 Running 状态。
func Start(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: 连接 SCM（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service: 打开服务 %s: %w", name, err)
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State == svc.Running {
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("service: 启动服务 %s: %w", name, err)
	}
	return waitState(s, svc.Running, serviceStartTimeout)
}

// Stop 停止服务并等待其进入 Stopped 状态。
func Stop(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: 连接 SCM（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service: 打开服务 %s: %w", name, err)
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("service: 查询服务 %s: %w", name, err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("service: 停止服务 %s: %w", name, err)
	}
	return waitState(s, svc.Stopped, serviceStopTimeout)
}

// Query 查询服务状态。
func Query(name string) (Status, error) {
	m, err := mgr.Connect()
	if err != nil {
		return Status{}, fmt.Errorf("service: 连接 SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return Status{}, fmt.Errorf("service: 打开服务 %s: %w", name, err)
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return Status{}, fmt.Errorf("service: 查询服务 %s: %w", name, err)
	}
	return Status{State: fromWindowsState(st.State), PID: st.ProcessId}, nil
}

// Run 以 Windows 服务方式运行 run，直到 run 返回或 SCM 请求停止/关机。
//
// SCM 回调内：启动时报告 StartPending → Running；Stop/Shutdown 时 cancel ctx，
// 等待 run 返回后报告 Stopped。run 返回的错误会作为 Run 的返回值。
func Run(name string, run func(ctx context.Context) error) error {
	if run == nil {
		return fmt.Errorf("service: run 回调不能为空")
	}
	h := &handler{run: run}
	err := svc.Run(name, h)
	if h.runErr != nil {
		return h.runErr
	}
	if err != nil {
		return fmt.Errorf("service: 运行服务 %s: %w", name, err)
	}
	return nil
}

// handler 实现 svc.Handler。
type handler struct {
	run    func(ctx context.Context) error
	runErr error
}

// Execute 是 SCM 控制回调。
func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			changes <- svc.Status{State: svc.StopPending}
			if err != nil && !errors.Is(err, context.Canceled) {
				h.runErr = err
				return false, 1
			}
			return false, 0
		case c, ok := <-r:
			if !ok {
				// SCM 关闭了控制通道：按停止处理，避免忙等。
				return h.stop(cancel, done)
			}
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				return h.stop(cancel, done)
			default:
				// 其它控制请求（Pause/Continue 等）不支持，忽略。
			}
		}
	}
}

// stop 取消 ctx 并等待业务退出，返回 SCM 期望的 (won'tStop, exitCode)。
func (h *handler) stop(cancel context.CancelFunc, done <-chan error) (bool, uint32) {
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			h.runErr = err
			return false, 1
		}
		return false, 0
	case <-time.After(runExitTimeout):
		h.runErr = fmt.Errorf("service: 进程未在 %v 内退出", runExitTimeout)
		return false, 1
	}
}

// IsWindowsService 报告当前进程是否由 SCM 启动。
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// fromWindowsState 把 SCM 状态映射为跨平台 State。
func fromWindowsState(s svc.State) State {
	switch s {
	case svc.Stopped:
		return StateStopped
	case svc.StartPending:
		return StateStartPending
	case svc.StopPending:
		return StateStopPending
	case svc.Running:
		return StateRunning
	case svc.Paused:
		return StatePaused
	default:
		return StateUnknown
	}
}

// waitState 轮询等待服务进入目标状态。
func waitState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("service: 查询服务状态: %w", err)
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service: 等待状态 %v 超时（当前 %v）", want, st.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
