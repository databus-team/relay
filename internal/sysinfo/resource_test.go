package sysinfo

import (
	"testing"
)

func TestCollectResourceInfo(t *testing.T) {
	info := CollectResourceInfo(false)
	if info.CPUCores <= 0 {
		t.Errorf("expected CPUCores > 0, got %v", info.CPUCores)
	}
	if info.MemTotalMB <= 0 {
		t.Errorf("expected MemTotalMB > 0, got %v", info.MemTotalMB)
	}

	infoUsage := CollectResourceInfo(true)
	if infoUsage.CPUCores <= 0 {
		t.Errorf("expected CPUCores > 0, got %v", infoUsage.CPUCores)
	}
	t.Logf("Resource (light): cpu=%s mem=%s", info.FormatCPUSummary(), info.FormatMemSummary())
	t.Logf("Resource (usage): cpu=%s mem=%s", infoUsage.FormatCPUUsage(), infoUsage.FormatMemUsage())
}
