//go:build soak && !linux && !windows

package e2e

import "runtime"

// processRSS 在不支持读系统 RSS 的平台回退为 Go 运行时的 MemStats.Sys（字节）。
func processRSS() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.Sys)
}

// processFDCount 在不支持统计 fd 的平台返回 0。
func processFDCount() int { return 0 }
