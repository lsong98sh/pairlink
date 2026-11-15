//go:build !windows

package config

import (
	"fmt"
	"os"
)

// syncDir 对目录调用 fsync，确保 os.Rename 产生的目录项在崩溃/掉电后仍然可见。
//
// 在类 Unix 系统上目录可以打开并 fsync（Linux 上真实落盘；某些文件系统如
// tmpfs 会接受调用但不落盘）。rename 只保证原子性，不保证持久性，因此这一步
// 是"写配置后不丢"的必要条件。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("打开目录: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("fsync 目录: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("关闭目录: %w", err)
	}
	return nil
}
