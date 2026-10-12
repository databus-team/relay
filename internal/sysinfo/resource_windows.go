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
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
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

// getCPUPercent 采样一个窗口内的系统忙时占比,返回 0-100。
func getCPUPercent() *float64 {
	var idle1, kernel1, user1 syscall.Filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle1)),
		uintptr(unsafe.Pointer(&kernel1)),
		uintptr(unsafe.Pointer(&user1)),
	)
	if ret == 0 {
		return nil
	}

	time.Sleep(cpuSampleInterval)

	var idle2, kernel2, user2 syscall.Filetime
	ret, _, _ = procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle2)),
		uintptr(unsafe.Pointer(&kernel2)),
		uintptr(unsafe.Pointer(&user2)),
	)
	if ret == 0 {
		return nil
	}

	idle := filetimeToUint64(idle2) - filetimeToUint64(idle1)
	kernel := filetimeToUint64(kernel2) - filetimeToUint64(kernel1)
	user := filetimeToUint64(user2) - filetimeToUint64(user1)

	// 在 Windows API 中,kernel 时间已经包含了 idle 时间
	total := kernel + user
	if total == 0 || total < idle {
		return nil
	}
	return percentPtr(ratioPercent(int64(total-idle), int64(total)))
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
		info.CPUPercent = getCPUPercent()
	}

	return info
}
