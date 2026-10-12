package sysinfo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// cpuSampleInterval 是 CPU 使用率的采样窗口。使用率只能由前后两次计数器差值算出,
// 窗口太短会因计数器精度不足而抖动(Windows 的 GetSystemTimes 有约 15ms 的 tick 粒度)。
const cpuSampleInterval = 200 * time.Millisecond

// ResourceInfo 描述一个节点的静态与动态资源信息
type ResourceInfo struct {
	CPUCores    float64  `json:"cpu_cores,omitempty"`    // 静态: CPU 核心配额 (如容器 2.0 核, 物理机 8 核)
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`  // 动态: 该配额的已用比例 (0-100); nil=未采集到
	MemTotalMB  int64    `json:"mem_total_mb,omitempty"` // 静态: 内存配额上限或物理总内存 (MB)
	MemUsedMB   int64    `json:"mem_used_mb,omitempty"`  // 动态: 当前内存已用量 (MB)
	IsContainer bool     `json:"is_container,omitempty"` // 是否处于容器环境 (如 K8s Pod / Docker)
}

// percentPtr 标记"这一项已经采到了":用 nil 表示未采集,才能和"恰好 0%"区分开
// (空闲机器在采样窗口内可以真的一纳秒都没忙)。
func percentPtr(v float64) *float64 { return &v }

// ratioPercent 把 (增量, 总量) 换算成 0-100 的占比。非正值视为无数据,返回 0。
func ratioPercent(used, total int64) float64 {
	if used <= 0 || total <= 0 {
		return 0
	}
	if used > total {
		used = total
	}
	return float64(used) * 100 / float64(total)
}

// formatCores 渲染核数:整数核不带小数,非整数核保留一位(如 2.5c)。
func formatCores(cores float64) string {
	if cores == math.Trunc(cores) {
		return fmt.Sprintf("%.0fc", cores)
	}
	return fmt.Sprintf("%.1fc", cores)
}

// FormatCPUSummary 格式化轻量静态展示: 例如 "8c" 或 "2c [pod]"
func (r *ResourceInfo) FormatCPUSummary() string {
	if r.CPUCores <= 0 {
		return "-"
	}
	res := formatCores(r.CPUCores)
	if r.IsContainer {
		res += " [pod]"
	}
	return res
}

// FormatMemSummary 格式化轻量静态展示: 例如 "16.0G" 或 "2.0G [pod]"
func (r *ResourceInfo) FormatMemSummary() string {
	if r.MemTotalMB <= 0 {
		return "-"
	}
	gb := float64(r.MemTotalMB) / 1024.0
	res := fmt.Sprintf("%.1fG", gb)
	if r.IsContainer {
		res += " [pod]"
	}
	return res
}

// FormatCPUUsage 格式化完整动态展示: 例如 "8c (40%)" 或 "2c (13%) [pod]"
func (r *ResourceInfo) FormatCPUUsage() string {
	if r.CPUCores <= 0 {
		return "-"
	}
	res := formatCores(r.CPUCores)
	if r.CPUPercent != nil {
		res += fmt.Sprintf(" (%.0f%%)", *r.CPUPercent)
	}
	if r.IsContainer {
		res += " [pod]"
	}
	return res
}

// FormatMemUsage 格式化完整动态展示: 例如 "4.2G / 16.0G (26%)"
func (r *ResourceInfo) FormatMemUsage() string {
	if r.MemTotalMB <= 0 {
		return "-"
	}
	totalGB := float64(r.MemTotalMB) / 1024.0
	if r.MemUsedMB > 0 {
		usedGB := float64(r.MemUsedMB) / 1024.0
		res := fmt.Sprintf("%.1fG/%.1fG (%s)", usedGB, totalGB, r.memPercent())
		if r.IsContainer {
			res += " [pod]"
		}
		return res
	}
	return r.FormatMemSummary()
}

// FormatCPUPercentCompact 只渲染 CPU 用量百分比(如 "40%"),供终端过窄时的降级展示:
// 完整形态 "8c (40%) [pod]" 一旦被截断,百分比最先丢掉,而它才是 --usage 要看的东西。
// 没采到用量时返回空串,让调用方退到省略号,而不是展示一个假的 0%。
func (r *ResourceInfo) FormatCPUPercentCompact() string {
	if r.CPUPercent == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *r.CPUPercent)
}

// FormatMemPercentCompact 只渲染内存用量百分比(如 "26%"),理由同 FormatCPUPercentCompact。
func (r *ResourceInfo) FormatMemPercentCompact() string {
	if r.MemUsedMB <= 0 {
		return ""
	}
	return r.memPercent()
}

// memPercent 渲染内存用量百分比,与 FormatMemUsage 里的百分比同源(截断而非四舍五入)。
func (r *ResourceInfo) memPercent() string {
	if r.MemTotalMB <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", int(float64(r.MemUsedMB)*100.0/float64(r.MemTotalMB)))
}

// parseProcStatCPU 累加 /proc/stat 首行(所有核)的计数,返回忙时与总时(jiffies)。
func parseProcStatCPU(data []byte) (busy, total int64, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var idle int64
		for i, field := range strings.Fields(line)[1:] {
			v, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return total - idle, total, total > 0
	}
	return 0, 0, false
}

// parseCgroupCPUUsec 从 cgroup v2 cpu.stat 取 usage_usec(本 cgroup 累计 CPU 微秒)。
func parseCgroupCPUUsec(data []byte) (int64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			if usec, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return usec, true
			}
		}
	}
	return 0, false
}
