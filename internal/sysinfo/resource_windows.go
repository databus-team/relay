//go:build windows

package sysinfo

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatus  = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
)

type memorystatusex struct {
	cbSize                  uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func filetimeToUint64(ft syscall.Filetime) uint64 {
	return (uint64(ft.HighDateTime) << 32) | uint64(ft.LowDateTime)
}

func getCPULoad() float64 {
	var idle1, kernel1, user1 syscall.Filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle1)),
		uintptr(unsafe.Pointer(&kernel1)),
		uintptr(unsafe.Pointer(&user1)),
	)
	if ret == 0 {
		return 0
	}

	time.Sleep(100 * time.Millisecond)

	var idle2, kernel2, user2 syscall.Filetime
	ret, _, _ = procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle2)),
		uintptr(unsafe.Pointer(&kernel2)),
		uintptr(unsafe.Pointer(&user2)),
	)
	if ret == 0 {
		return 0
	}

	i1 := filetimeToUint64(idle1)
	k1 := filetimeToUint64(kernel1)
	u1 := filetimeToUint64(user1)

	i2 := filetimeToUint64(idle2)
	k2 := filetimeToUint64(kernel2)
	u2 := filetimeToUint64(user2)

	idle := i2 - i1
	kernel := k2 - k1
	user := u2 - u1

	// 在 Windows API 中，kernel 时间已经包含了 idle 时间
	total := kernel + user
	if total == 0 || total < idle {
		return 0
	}

	busy := total - idle
	cores := float64(runtime.NumCPU())
	// 换算为类 Unix load 指标 (busy / total * cores)，例如 8 核满载为 8.0，与 Linux/macOS 保持统一标准
	load := (float64(busy) / float64(total)) * cores
	return load
}

// CollectResourceInfo 收集 Windows 平台资源
func CollectResourceInfo(withUsage bool) ResourceInfo {
	info := ResourceInfo{
		CPUCores: float64(runtime.NumCPU()),
	}

	var mem memorystatusex
	mem.cbSize = uint32(unsafe.Sizeof(mem))

	ret, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&mem)))
	if ret != 0 {
		info.MemTotalMB = int64(mem.ullTotalPhys / (1024 * 1024))
		if withUsage {
			info.MemUsedMB = int64((mem.ullTotalPhys - mem.ullAvailPhys) / (1024 * 1024))
		}
	}

	if withUsage {
		info.CPULoad = getCPULoad()
	}

	return info
}

