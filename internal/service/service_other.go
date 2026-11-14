//go:build !windows

package service

import "context"

// Install 在非 Windows 平台返回 ErrUnsupported。
func Install(name, displayName string, args []string) error { return ErrUnsupported }

// Uninstall 在非 Windows 平台返回 ErrUnsupported。
func Uninstall(name string) error { return ErrUnsupported }

// Start 在非 Windows 平台返回 ErrUnsupported。
func Start(name string) error { return ErrUnsupported }

// Stop 在非 Windows 平台返回 ErrUnsupported。
func Stop(name string) error { return ErrUnsupported }

// Query 在非 Windows 平台返回 ErrUnsupported。
func Query(name string) (Status, error) { return Status{}, ErrUnsupported }

// Run 在非 Windows 平台返回 ErrUnsupported。
func Run(name string, run func(ctx context.Context) error) error { return ErrUnsupported }

// IsWindowsService 在非 Windows 平台恒为 false。
func IsWindowsService() bool { return false }
