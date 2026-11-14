// Package service 封装 Windows 服务（SCM）的安装、卸载、启动、停止与运行。
//
// Windows 实现见 service_windows.go；非 Windows 平台的所有函数返回 ErrUnsupported，
// 从而保证同一份代码跨平台可编译。
//
// 服务名固定为 "pairlink-<mode>"（如 pairlink-relay），由 Name 生成。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupported 表示当前平台不支持 Windows 服务管理。
var ErrUnsupported = errors.New("service: 当前平台不支持 Windows 服务管理（仅 Windows 可用）")

// State 是服务状态，与 SCM 状态一一对应但跨平台可引用。
type State int

// 服务状态枚举。
const (
	// StateUnknown 表示未知状态（服务不存在或查询失败）。
	StateUnknown State = iota
	// StateStopped 表示服务已停止。
	StateStopped
	// StateStartPending 表示服务正在启动。
	StateStartPending
	// StateStopPending 表示服务正在停止。
	StateStopPending
	// StateRunning 表示服务正在运行。
	StateRunning
	// StatePaused 表示服务已暂停。
	StatePaused
)

// String 实现 fmt.Stringer。
func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStartPending:
		return "start-pending"
	case StateStopPending:
		return "stop-pending"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	default:
		return "unknown"
	}
}

// Status 是服务状态查询结果。
type Status struct {
	// State 是服务状态。
	State State
	// PID 是服务进程 ID（未运行时为 0）。
	PID uint32
}

// Name 返回模式对应的固定服务名："pairlink-<mode>"。
func Name(mode string) string {
	return "pairlink-" + strings.ToLower(strings.TrimSpace(mode))
}

// LogPath 返回 -service 模式下的默认日志文件路径：exe 同目录的 <name>.log。
//
// Windows 服务没有控制台，日志必须落文件或事件日志；这里统一落文件。
func LogPath(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("service: 获取可执行文件路径: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), name+".log"), nil
}
