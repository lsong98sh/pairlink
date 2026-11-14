//go:build soak && linux

package e2e

import (
	"os"
	"strconv"
	"strings"
)

// processRSS 返回当前进程的常驻内存（RSS，字节）；读不到返回 0。
func processRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// processFDCount 返回当前进程打开的文件描述符数量。
func processFDCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}
