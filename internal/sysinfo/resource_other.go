//go:build !linux && !darwin && !windows

package sysinfo

import (
	"runtime"
)

// CollectResourceInfo 兜底其他平台
func CollectResourceInfo(withUsage bool) ResourceInfo {
	return ResourceInfo{
		CPUCores: float64(runtime.NumCPU()),
	}
}
