//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
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

		// CPU 使用率
		info.CPUPercent = collectCPUPercent(isContainer, info.CPUCores)
	}

	return info
}

// collectCPUPercent 采样 CPU 使用率(占配额的百分比)。
// 容器内必须用 cgroup 自身计数: /proc/stat 与 /proc/loadavg 都不做 namespace 隔离,
// 读到的整机口径拿去跟 cgroup 配额相除,会把宿主机负载算到 Pod 头上。
func collectCPUPercent(isContainer bool, cores float64) *float64 {
	if isContainer {
		start := time.Now()
		used0, ok := cgroupCPUUsec()
		if !ok {
			return nil
		}
		time.Sleep(cpuSampleInterval)
		used1, ok := cgroupCPUUsec()
		if !ok {
			return nil
		}
		elapsedUs := time.Since(start).Microseconds()
		return percentPtr(ratioPercent(used1-used0, int64(float64(elapsedUs)*cores)))
	}

	busy0, total0, ok := readProcStatCPU()
	if !ok {
		return nil
	}
	time.Sleep(cpuSampleInterval)
	busy1, total1, ok := readProcStatCPU()
	if !ok {
		return nil
	}
	return percentPtr(ratioPercent(busy1-busy0, total1-total0))
}

// cgroupCPUUsec 读本 cgroup 的累计 CPU 用时(微秒),即"用了多少核·微秒"。
func cgroupCPUUsec() (int64, bool) {
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
		if usec, ok := parseCgroupCPUUsec(data); ok {
			return usec, true
		}
	}
	// cgroup v1: cpuacct.usage 单位纳秒
	if data, err := os.ReadFile("/sys/fs/cgroup/cpuacct/cpuacct.usage"); err == nil {
		if ns, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return ns / 1000, true
		}
	}
	return 0, false
}

func readProcStatCPU() (busy, total int64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseProcStatCPU(data)
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
