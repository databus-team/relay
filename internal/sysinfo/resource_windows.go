//go:build windows

package sysinfo

import (
	"runtime"
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
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
			info.CPULoad = float64(mem.dwMemoryLoad) // Windows 内存负载百分比兜底
		}
	}

	return info
}
