//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// CollectResourceInfo 收集 Linux 平台资源。
// 优先探测 cgroup v2 与 cgroup v1，准确区分 K8s Pod / Docker 容器配额与宿主机物理资源。
func CollectResourceInfo(withUsage bool) ResourceInfo {
	info := ResourceInfo{
		CPUCores: float64(runtime.NumCPU()),
	}

	isContainer := false

	// 1. 内存 Limit & cgroup 探测
	// 尝试 cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		str := strings.TrimSpace(string(data))
		if str != "max" {
			if limitBytes, err := strconv.ParseInt(str, 10, 64); err == nil && limitBytes > 0 {
				info.MemTotalMB = limitBytes / (1024 * 1024)
				isContainer = true
			}
		}
	}

	// 尝试 cgroup v1 (若 v2 未检出 limit)
	if info.MemTotalMB <= 0 {
		if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
			str := strings.TrimSpace(string(data))
			if limitBytes, err := strconv.ParseInt(str, 10, 64); err == nil && limitBytes > 0 {
				// cgroup v1 中如果未限制，通常是一个超大的数 (如 0x7FFFFFFFFFFFF000 = 9223372036854771712)
				if limitBytes < 100*1024*1024*1024*1024 { // 小于 100TB 说明被显式配置了 limit
					info.MemTotalMB = limitBytes / (1024 * 1024)
					isContainer = true
				}
			}
		}
	}

	// 若未在 cgroup 中限制，读取宿主机 /proc/meminfo 的 MemTotal
	memTotalFromProc, memAvailFromProc := readProcMeminfo()
	if info.MemTotalMB <= 0 && memTotalFromProc > 0 {
		info.MemTotalMB = memTotalFromProc
	}

	// 2. CPU Limit & cgroup 探测
	// 尝试 cgroup v2: cpu.max 格式为 "quota period"，如 "200000 100000"
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 && fields[0] != "max" {
			quota, qErr := strconv.ParseFloat(fields[0], 64)
			period, pErr := strconv.ParseFloat(fields[1], 64)
			if qErr == nil && pErr == nil && period > 0 {
				info.CPUCores = quota / period
				isContainer = true
			}
		}
	}

	// 尝试 cgroup v1: cpu.cfs_quota_us 和 cpu.cfs_period_us
	if !isContainer {
		quotaData, qErr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		periodData, pErr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		if qErr == nil && pErr == nil {
			quota, qErr := strconv.ParseFloat(strings.TrimSpace(string(quotaData)), 64)
			period, pErr := strconv.ParseFloat(strings.TrimSpace(string(periodData)), 64)
			if qErr == nil && pErr == nil && quota > 0 && period > 0 {
				info.CPUCores = quota / period
				isContainer = true
			}
		}
	}

	// 判断是否在 K8s 容器环境的其他特征
	if !isContainer {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			isContainer = true
		} else if _, err := os.Stat("/var/run/secrets/kubernetes.io"); err == nil {
			isContainer = true
		}
	}
	info.IsContainer = isContainer

	// 3. 动态用量采集 (当 withUsage 为 true 时)
	if withUsage {
		// 内存当前已用量
		if isContainer {
			// cgroup v2
			if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
				if curBytes, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
					info.MemUsedMB = curBytes / (1024 * 1024)
				}
			}
			// cgroup v1
			if info.MemUsedMB <= 0 {
				if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
					if curBytes, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
						info.MemUsedMB = curBytes / (1024 * 1024)
					}
				}
			}
		}

		if info.MemUsedMB <= 0 && memTotalFromProc > 0 && memAvailFromProc > 0 {
			info.MemUsedMB = memTotalFromProc - memAvailFromProc
		}

		// CPU 负载 (读取 /proc/loadavg)
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
					info.CPULoad = load
				}
			}
		}
	}

	return info
}

func readProcMeminfo() (totalMB int64, availMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					totalMB = kb / 1024
				}
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					availMB = kb / 1024
				}
			}
		}
	}
	return totalMB, availMB
}
