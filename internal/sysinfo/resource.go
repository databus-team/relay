package sysinfo

import (
	"fmt"
	"math"
)

// ResourceInfo 描述一个节点的静态与动态资源信息
type ResourceInfo struct {
	CPUCores    float64 `json:"cpu_cores,omitempty"`    // CPU 核心配额 (如容器 2.0 核, 物理机 8 核)
	CPULoad     float64 `json:"cpu_load,omitempty"`     // 动态: CPU 1 分钟平均负载或瞬时百分比
	MemTotalMB  int64   `json:"mem_total_mb,omitempty"` // 静态: 内存配额上限或物理总内存 (MB)
	MemUsedMB   int64   `json:"mem_used_mb,omitempty"`  // 动态: 当前内存已用量 (MB)
	IsContainer bool    `json:"is_container,omitempty"` // 是否处于容器环境 (如 K8s Pod / Docker)
}

// FormatCPUSummary 格式化轻量静态展示: 例如 "8c" 或 "2c [pod]"
func (r *ResourceInfo) FormatCPUSummary() string {
	if r.CPUCores <= 0 {
		return "-"
	}
	cores := ""
	if r.CPUCores == math.Trunc(r.CPUCores) {
		cores = fmt.Sprintf("%.0fc", r.CPUCores)
	} else {
		cores = fmt.Sprintf("%.1fc", r.CPUCores)
	}
	if r.IsContainer {
		cores += " [pod]"
	}
	return cores
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

// FormatCPUUsage 格式化完整动态展示: 例如 "8c (2.5)" 或 "2c (0.3) [pod]"
func (r *ResourceInfo) FormatCPUUsage() string {
	summary := r.FormatCPUSummary()
	if summary == "-" {
		return "-"
	}
	if r.CPULoad > 0 {
		if r.IsContainer {
			// 去掉后面的 " [pod]" 先插入 load
			base := fmt.Sprintf("%.0fc", r.CPUCores)
			if r.CPUCores != math.Trunc(r.CPUCores) {
				base = fmt.Sprintf("%.1fc", r.CPUCores)
			}
			return fmt.Sprintf("%s (%.1f) [pod]", base, r.CPULoad)
		}
		return fmt.Sprintf("%s (%.1f)", summary, r.CPULoad)
	}
	return summary
}

// FormatMemUsage 格式化完整动态展示: 例如 "4.2G / 16.0G (26%)"
func (r *ResourceInfo) FormatMemUsage() string {
	if r.MemTotalMB <= 0 {
		return "-"
	}
	totalGB := float64(r.MemTotalMB) / 1024.0
	if r.MemUsedMB > 0 {
		usedGB := float64(r.MemUsedMB) / 1024.0
		pct := int(float64(r.MemUsedMB) * 100.0 / float64(r.MemTotalMB))
		res := fmt.Sprintf("%.1fG/%.1fG (%d%%)", usedGB, totalGB, pct)
		if r.IsContainer {
			res += " [pod]"
		}
		return res
	}
	return r.FormatMemSummary()
}
