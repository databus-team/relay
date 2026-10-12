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
		info.CPUPercent = collectCPUPercent()

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

// collectCPUPercent 用 iostat 取一个采样窗口内的系统忙时占比(user+sys)。
// macOS 无 cgroup,且 Apple Silicon 上没有 kern.cp_time,纯 Go 取不到瞬时利用率;
// loadavg 既含不可中断睡眠、又只有 1 分钟均值,实测会明显高于真实占用,故不用它。
// iostat 首行是开机至今的均值,只能取末行;-n 0 去掉磁盘列,让 us/sy 落在行首。
func collectCPUPercent() *float64 {
	out, err := exec.Command("iostat", "-c", "2", "-w", "1", "-n", "0").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 2 {
		return nil
	}
	user, uErr := strconv.ParseFloat(fields[0], 64)
	sys, sErr := strconv.ParseFloat(fields[1], 64)
	if uErr != nil || sErr != nil {
		return nil
	}
	return percentPtr(user + sys)
}
