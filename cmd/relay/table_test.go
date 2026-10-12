package main

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/user/relay/internal/sysinfo"
)

// TestCellWidth 覆盖显示宽度:ASCII 1 列、CJK/全角 2 列、组合符 0 列。按 rune 数算宽度会让
// 含中文的单元格撑破表宽,行照样换行 —— 这正是要修的问题。
func TestCellWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"relay", 5},
		{"中文", 4},
		{"中文abc", 7},
		{"ｆｕｌｌ", 8},
		{"あア漢", 6},
		{"cafe\u0301", 4}, // e + 组合重音
		{"a\u200bb", 2},   // 零宽空格
	}
	for _, c := range cases {
		if got := cellWidth(c.in); got != c.want {
			t.Errorf("cellWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestTruncateToWidth 覆盖单元格收窄:够宽原样返回,超宽按显示宽度裁并保留合法 UTF-8。
func TestTruncateToWidth(t *testing.T) {
	if got := truncateToWidth("8080:9000", psPortWidth); got != "8080:9000" {
		t.Errorf("truncateToWidth(short) = %q, want unchanged", got)
	}
	if got := truncateToWidth("8080:9000,9090:10000", psPortWidth); got != "8080:9000,909..." {
		t.Errorf("truncateToWidth(long) = %q, want %q", got, "8080:9000,909...")
	}
	// 中文一个字占两列:裁到 16 列只能放下 6 个字 + 省略号(而不是 13 个 rune)。
	got := truncateToWidth(strings.Repeat("中文主机", 5), psPortWidth)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateToWidth(unicode) = %q, want valid UTF-8", got)
	}
	if w := cellWidth(got); w > psPortWidth {
		t.Errorf("truncateToWidth(unicode) = %q (%d cells), want <= %d", got, w, psPortWidth)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncateToWidth(unicode) = %q, want an ellipsis", got)
	}
	// 极窄:省略号自身都放不下时只用点号填满,仍然不超宽。
	if got := truncateToWidth("abcdef", 2); got != ".." {
		t.Errorf("truncateToWidth(width=2) = %q, want ..", got)
	}
	if got := truncateToWidth("abcdef", 0); got != "" {
		t.Errorf("truncateToWidth(width=0) = %q, want empty", got)
	}
}

// sampleCols 是渲染器测试用的列:定宽小列 + 一个自由文本末列。
func sampleCols() []tableCol {
	return []tableCol{
		{Title: "ID", Min: 3},
		{Title: "NODE", Min: 5, Max: 12},
		{Title: "COMMAND", Min: 6},
	}
}

// TestRenderTableNoWrap 钉住核心不变量:给定终端宽度,每一行的显示宽度都不超过它,
// 且确有省略时 truncated 为真。
func TestRenderTableNoWrap(t *testing.T) {
	rows := [][]tableCell{
		{textCell("t-1"), textCell("gpu-node-01.internal"), textCell("python3 -m uvicorn app.main:app --host 0.0.0.0 --port 8000")},
		{textCell("t-2"), textCell("win-01"), textCell("cmd /c run.bat")},
	}
	for _, width := range []int{40, 60, 80, 120} {
		lines, _ := renderTable(width, sampleCols(), rows)
		for _, l := range lines {
			if w := cellWidth(l); w > width {
				t.Errorf("width=%d: line is %d cells wide: %q", width, w, l)
			}
		}
	}
	lines, truncated := renderTable(40, sampleCols(), rows)
	if !truncated {
		t.Errorf("width=40: expected truncation, got %q", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "...") {
		t.Errorf("width=40: expected an ellipsis somewhere in %q", lines)
	}
}

// TestRenderTableUnknownWidthKeepsEverything 覆盖重定向/管道的路径:宽度未知时不省任何字符,
// 列宽按内容取自然宽。
func TestRenderTableUnknownWidthKeepsEverything(t *testing.T) {
	cmd := "python3 -m uvicorn app.main:app --host 0.0.0.0 --port 8000"
	rows := [][]tableCell{{textCell("t-1"), textCell("gpu-node-01.internal"), textCell(cmd)}}
	lines, truncated := renderTable(0, sampleCols(), rows)
	if truncated {
		t.Errorf("width=0 must not truncate: %q", lines)
	}
	if !strings.HasSuffix(lines[1], cmd) {
		t.Errorf("row = %q, want the full command", lines[1])
	}
	// 列宽按内容对齐:表头与数据行的第三列必须在同一列开始。
	if strings.Index(lines[0], "COMMAND") != strings.Index(lines[1], cmd) {
		t.Errorf("columns misaligned:\n%s\n%s", lines[0], lines[1])
	}
}

// TestRenderTablePrefersCompact 覆盖降级顺序:列放不下完整形态时先试 Compact(保住关键指标),
// 只有它也放不下才用省略号。这里 CPU 没有 Compact(退到省略号),MEMORY 有(整格保住)。
func TestRenderTablePrefersCompact(t *testing.T) {
	cols := []tableCol{{Title: "CPU", Min: 3}, {Title: "MEMORY", Min: 3}}
	rows := [][]tableCell{
		{textCell("8c (40%) [pod]"), {Full: "15.6G/31.2G (26%)", Compact: "26%"}},
	}
	lines, truncated := renderTable(12, cols, rows)
	if !truncated {
		t.Fatalf("expected truncation, got %q", lines)
	}
	if !strings.HasSuffix(lines[1], "26%") {
		t.Errorf("row = %q, want the memory cell kept as its compact percent form", lines[1])
	}
	if strings.Contains(lines[1], "15.6G") {
		t.Errorf("row = %q, want the full memory form dropped", lines[1])
	}
	if !strings.Contains(lines[0], "MEMORY") {
		t.Errorf("header = %q, want the full column title", lines[0])
	}
}

// TestRenderTableKeepsHeaderWidth 覆盖下限规则:表头只有几个字符,收窄时不该先牺牲它 ——
// 一列被省略到看不懂列名,读者就没法读表了。
func TestRenderTableKeepsHeaderWidth(t *testing.T) {
	cols := []tableCol{{Title: "CREATED", Min: 4}, {Title: "COMMAND", Min: 4}}
	rows := [][]tableCell{{textCell("1718000000"), textCell("some long command line")}}
	lines, _ := renderTable(30, cols, rows)
	if !strings.HasPrefix(lines[0], "CREATED") {
		t.Errorf("header = %q, want CREATED kept whole", lines[0])
	}
}

// TestRenderTableHardTruncate 覆盖最后的兜底:各列下限(表头保底)之和本身就超过终端宽度时,
// 整行被硬裁 —— 保证不换行比保住末列更重要。
func TestRenderTableHardTruncate(t *testing.T) {
	cols := []tableCol{{Title: "AAAAAAAAAA", Min: 10}, {Title: "BBBBBBBBBB", Min: 10}, {Title: "CCCCCCCCCC", Min: 10}}
	rows := [][]tableCell{{textCell("1111111111"), textCell("2222222222"), textCell("3333333333")}}
	lines, truncated := renderTable(12, cols, rows)
	if !truncated {
		t.Fatalf("expected truncation, got %q", lines)
	}
	for _, l := range lines {
		if w := cellWidth(l); w > 12 {
			t.Errorf("line is %d cells wide, want <= 12: %q", w, l)
		}
	}
}

// TestNodesTableUsageKeepsPercent 钉住 relay nodes -u 的诉求:半屏宽度下 CPU/MEMORY 降级成百分比,
// 而不是把 "--usage" 唯一想看的东西截掉。
func TestNodesTableUsageKeepsPercent(t *testing.T) {
	nodes := []PeerNodeItem{{
		ID:         "gpu-node-01.internal (local)",
		Role:       "hub,agent",
		Status:     "Ready",
		Platform:   "darwin/arm64",
		CPU:        "8c (40%) [pod]",
		Memory:     "15.6G/31.2G (26%) [pod]",
		Version:    "v0.4.5+13f288f",
		Latency:    "N/A (executor offline)",
		cpuCompact: "40%",
		memCompact: "26%",
	}}

	const width = 70
	lines, truncated := renderNodesTable(nodes, width)
	if !truncated {
		t.Fatalf("expected truncation at width %d, got %q", width, lines)
	}
	for _, l := range lines {
		if w := cellWidth(l); w > width {
			t.Errorf("line is %d cells wide (terminal %d): %q", w, width, l)
		}
	}
	row := lines[1]
	if !strings.Contains(row, "40%") || !strings.Contains(row, "26%") {
		t.Errorf("row = %q, want both usage percentages to survive", row)
	}
}

// TestTermWidthNonTTY 钉住探测失败的处理:管道不是终端,探测返回 0 —— 调用方据此按「宽度未知」
// 渲染,而不是猜一个宽度去省略内容(重定向输出必须无损)。
func TestTermWidthNonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if got := termWidth(w); got != 0 {
		t.Errorf("termWidth(pipe) = %d, want 0", got)
	}
}

// TestOutputWidthOverride 覆盖测试接缝的优先级:接缝 > TTY > COLUMNS。
func TestOutputWidthOverride(t *testing.T) {
	termWidthOverride = 123
	defer func() { termWidthOverride = 0 }()
	if got := outputWidth(); got != 123 {
		t.Errorf("outputWidth() = %d, want the override 123", got)
	}
}

// TestFormatNodeResourceCompact 覆盖资源格的降级形态:只在 -u 且有动态值时给出,否则留空
// (留空 = 交给省略号,而不是展示一个假的 0%)。
func TestFormatNodeResourceCompact(t *testing.T) {
	pct := 40.0
	withUsage := sysinfo.ResourceInfo{CPUCores: 8, CPUPercent: &pct, MemTotalMB: 32000, MemUsedMB: 8320, IsContainer: true}

	cpu, mem := formatNodeResourceCompact(withUsage, false)
	if cpu != "" || mem != "" {
		t.Errorf("without -u: got (%q, %q), want empty", cpu, mem)
	}
	cpu, mem = formatNodeResourceCompact(withUsage, true)
	if cpu != "40%" || mem != "26%" {
		t.Errorf("with -u: got (%q, %q), want (40%%, 26%%)", cpu, mem)
	}

	noSamples := sysinfo.ResourceInfo{CPUCores: 8, MemTotalMB: 32000}
	if cpu, mem := formatNodeResourceCompact(noSamples, true); cpu != "" || mem != "" {
		t.Errorf("no samples: got (%q, %q), want empty", cpu, mem)
	}
}
