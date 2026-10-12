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
	if infoUsage.CPUPercent == nil {
		t.Error("expected CPUPercent to be measured")
	} else if *infoUsage.CPUPercent < 0 || *infoUsage.CPUPercent > 100 {
		t.Errorf("expected CPUPercent within 0-100, got %v", *infoUsage.CPUPercent)
	}
	t.Logf("Resource (light): cpu=%s mem=%s", info.FormatCPUSummary(), info.FormatMemSummary())
	t.Logf("Resource (usage): cpu=%s mem=%s", infoUsage.FormatCPUUsage(), infoUsage.FormatMemUsage())
}

func TestFormatCPU(t *testing.T) {
	cases := []struct {
		name string
		res  ResourceInfo
		want string
	}{
		{"未采集", ResourceInfo{CPUCores: 8}, "8c"},
		{"整数核百分比", ResourceInfo{CPUCores: 8, CPUPercent: percentPtr(40.4)}, "8c (40%)"},
		{"恰好 0%", ResourceInfo{CPUCores: 8, CPUPercent: percentPtr(0)}, "8c (0%)"},
		{"非整数核", ResourceInfo{CPUCores: 2.5, CPUPercent: percentPtr(13)}, "2.5c (13%)"},
		{"容器", ResourceInfo{CPUCores: 2, CPUPercent: percentPtr(7), IsContainer: true}, "2c (7%) [pod]"},
		{"容器未采集", ResourceInfo{CPUCores: 2, IsContainer: true}, "2c [pod]"},
		{"未知核数", ResourceInfo{CPUPercent: percentPtr(40)}, "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.FormatCPUUsage(); got != tc.want {
				t.Errorf("FormatCPUUsage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRatioPercent(t *testing.T) {
	cases := []struct {
		used, total int64
		want        float64
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 0, 0},
		{-5, 100, 0},
		{150, 100, 100},
	}
	for _, tc := range cases {
		if got := ratioPercent(tc.used, tc.total); got != tc.want {
			t.Errorf("ratioPercent(%d, %d) = %v, want %v", tc.used, tc.total, got, tc.want)
		}
	}
}

func TestParseProcStatCPU(t *testing.T) {
	// 字段序: user nice system idle iowait irq softirq steal guest guest_nice
	busy1, total1, ok := parseProcStatCPU([]byte("cpu  100 0 50 800 50 10 5 0 0 0\ncpu0 50 0 25 400 25 5 2 0 0 0\nintr 123\n"))
	if !ok {
		t.Fatal("expected ok")
	}
	if total1 != 1015 {
		t.Errorf("total = %d, want 1015", total1)
	}
	if busy1 != 165 { // total - idle - iowait = 1015 - 800 - 50
		t.Errorf("busy = %d, want 165", busy1)
	}
	if _, _, ok := parseProcStatCPU([]byte("intr 1\nctxt 2\n")); ok {
		t.Error("expected no cpu line to fail")
	}

	// 只有 idle+iowait 增长 -> 0%
	busy2, total2, _ := parseProcStatCPU([]byte("cpu  100 0 50 900 50 10 5 0 0 0\n"))
	if got := ratioPercent(busy2-busy1, total2-total1); got != 0 {
		t.Errorf("idle-only delta = %v, want 0", got)
	}
	// user 增长 110,同窗口总时也增长 110 -> 100%
	busy3, total3, _ := parseProcStatCPU([]byte("cpu  210 0 50 900 50 10 5 0 0 0\n"))
	if got := ratioPercent(busy3-busy2, total3-total2); got != 100 {
		t.Errorf("busy delta = %v, want 100", got)
	}
}

func TestParseCgroupCPUUsec(t *testing.T) {
	v2 := []byte("usage_usec 1234567\nuser_usec 900000\nsystem_usec 334567\nnr_periods 10\n")
	if usec, ok := parseCgroupCPUUsec(v2); !ok || usec != 1234567 {
		t.Errorf("parseCgroupCPUUsec(v2) = %d, %v; want 1234567, true", usec, ok)
	}
	// cgroup v1 的 cpu.stat 没有 usage_usec,应回落到 cpuacct.usage
	v1 := []byte("nr_periods 10\nnr_throttled 0\nthrottled_time 0\n")
	if _, ok := parseCgroupCPUUsec(v1); ok {
		t.Error("expected v1 cpu.stat to have no usage_usec")
	}
}

func TestFormatMemUsage(t *testing.T) {
	res := ResourceInfo{MemTotalMB: 16384, MemUsedMB: 4096}
	if got := res.FormatMemUsage(); got != "4.0G/16.0G (25%)" {
		t.Errorf("FormatMemUsage() = %q", got)
	}
	res.IsContainer = true
	if got := res.FormatMemUsage(); got != "4.0G/16.0G (25%) [pod]" {
		t.Errorf("FormatMemUsage() = %q", got)
	}
}
