//go:build darwin

package sysinfo

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// CollectResourceInfo 收集 macOS 平台资源
func CollectResourceInfo(withUsage bool) ResourceInfo {
	info := ResourceInfo{
		CPUCores: float64(runtime.NumCPU()),
	}

	// 内存总量 (hw.memsize)
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if bytesTotal, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			info.MemTotalMB = bytesTotal / (1024 * 1024)
		}
	}

	if withUsage {
		// CPU 负载 (vm.loadavg)
		if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
			// 格式: { 2.59 3.60 3.69 }
			cleaned := strings.Trim(strings.TrimSpace(string(out)), "{ }")
			fields := strings.Fields(cleaned)
			if len(fields) > 0 {
				if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
					info.CPULoad = load
				}
			}
		}

		// 内存使用量 (通过 vm_stat 粗略估算 active + wired 内存)
		if out, err := exec.Command("vm_stat").Output(); err == nil {
			var pageSize int64 = 4096
			var activePages, wiredPages, compressedPages int64
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				parts := strings.Split(line, ":")
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					valStr := strings.Trim(strings.TrimSpace(parts[1]), ".")
					val, _ := strconv.ParseInt(valStr, 10, 64)
					switch key {
					case "Pages active":
						activePages = val
					case "Pages wired down":
						wiredPages = val
					case "Pages occupied by compressor":
						compressedPages = val
					}
				}
			}
			usedBytes := (activePages + wiredPages + compressedPages) * pageSize
			info.MemUsedMB = usedBytes / (1024 * 1024)
		}
	}

	return info
}
