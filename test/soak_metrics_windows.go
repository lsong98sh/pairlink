//go:build soak && windows

package e2e

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters 对应 Win32 的 PROCESS_MEMORY_COUNTERS。
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	modkernel32               = syscall.NewLazyDLL("kernel32.dll")
	modpsapi                  = syscall.NewLazyDLL("psapi.dll")
	procGetCurrentProcess     = modkernel32.NewProc("GetCurrentProcess")
	procGetProcessHandleCount = modkernel32.NewProc("GetProcessHandleCount")
	procGetProcessMemoryInfo  = modpsapi.NewProc("GetProcessMemoryInfo")
)

// currentProcessHandle 返回当前进程的伪句柄。
func currentProcessHandle() uintptr {
	h, _, _ := procGetCurrentProcess.Call()
	return h
}

// processRSS 返回当前进程的常驻内存（WorkingSetSize，字节）；失败返回 0。
func processRSS() int64 {
	var m processMemoryCounters
	m.Cb = uint32(unsafe.Sizeof(m))
	ok, _, _ := procGetProcessMemoryInfo.Call(
		currentProcessHandle(),
		uintptr(unsafe.Pointer(&m)),
		uintptr(m.Cb),
	)
	if ok == 0 {
		return 0
	}
	return int64(m.WorkingSetSize)
}

// processFDCount 返回当前进程的句柄数量（Windows 上等价于 fd 计数）。
func processFDCount() int {
	var n uint32
	ok, _, _ := procGetProcessHandleCount.Call(
		currentProcessHandle(),
		uintptr(unsafe.Pointer(&n)),
	)
	if ok == 0 {
		return 0
	}
	return int(n)
}
