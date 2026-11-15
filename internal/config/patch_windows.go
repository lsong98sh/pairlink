//go:build windows

package config

// syncDir 在 Windows 上是空操作。
//
// Windows 没有"对目录 fsync"的等价操作：FlushFileBuffers 只接受文件句柄，
// 对目录句柄调用会返回 ERROR_INVALID_HANDLE。NTFS 的目录项更新由文件系统
// 日志（$LogFile）保证，语义弱于 Linux 的 fsync(dir)。这里跳过而不是返回错误，
// 否则所有写配置在 Windows 上都会失败。
func syncDir(dir string) error {
	_ = dir
	return nil
}
