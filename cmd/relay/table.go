package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ellipsis 是省略标记。沿用历史输出的三点,而不是单字符 "…":同样的省略在不同终端/字体下的
// 宽度更可预期,读者也早已习惯。
const ellipsis = "..."

// tableCell 是一格的内容。Compact 是这一列被收窄时的降级形态:渲染器在 Full 放不下时会先试它
// (如 -u 的内存 "15.6G/31.2G (50%)" 降级成 "50%",先保住动态指标),它也放不下才退到省略号。
type tableCell struct {
	Full    string
	Compact string
}

// textCell 是只有一种形态的格子。
func textCell(s string) tableCell { return tableCell{Full: s} }

// tableCol 描述一列。Min 是收窄下限,Max 是自然宽上限(0 = 不限)。表头宽度永远保底 —— 表头被
// 省略会让人读不懂这是哪一列,而它只有几个字符,不值得为它省宽度。
type tableCol struct {
	Title string
	Min   int
	Max   int
}

// renderTable 渲染一张「永不换行」的表:先按内容取各列自然宽,总宽超出 width 时按各列可让出的
// 容量等比收窄(不破各自下限),仍放不下就由末列继续吃掉溢出,最后再兜一层整行硬裁。返回的
// truncated 表示确有内容被终端宽度省略(用了 Compact 或省略号),调用方据此提示完整内容的来源;
// 各列自己的定宽上限(Max,如 PORTS)是无条件的历史行为,不计入。
//
// width<=0 表示宽度未知(重定向/管道/探测失败),此时按自然宽渲染、一个字符都不省:输出被
// 重定向到文件或管道时不该受终端尺寸影响,机器消费者另有 --json。
func renderTable(width int, cols []tableCol, rows [][]tableCell) ([]string, bool) {
	n := len(cols)
	if n == 0 {
		return nil, false
	}

	natural := make([]int, n)
	for j, c := range cols {
		w := cellWidth(c.Title)
		for _, row := range rows {
			if j < len(row) {
				w = max(w, cellWidth(row[j].Full))
			}
		}
		if c.Max > 0 {
			w = min(w, c.Max)
		}
		natural[j] = w
	}

	widths := natural
	enforced := false
	if width > 0 && tableWidth(natural) > width {
		floors := make([]int, n)
		for j, c := range cols {
			floors[j] = min(natural[j], max(c.Min, cellWidth(c.Title)))
		}
		widths = shrinkWidths(natural, floors, width)
		enforced = true
	}

	header := make([]tableCell, n)
	for j, c := range cols {
		header[j] = textCell(c.Title)
	}
	out := make([]string, 0, len(rows)+1)
	truncated := false
	for _, row := range append([][]tableCell{header}, rows...) {
		line, cut := renderTableRow(widths, row, width)
		out = append(out, line)
		// 只有宽度受限的布局才算「按终端宽度省略」:定宽上限(Max,如 PORTS)是无条件的历史
		// 行为,不该在输出被重定向时也喊一声省略。
		if cut && enforced {
			truncated = true
		}
	}
	return out, truncated
}

// tableWidth 是一行表格的显示宽度:各列宽 + 列间单空格。
func tableWidth(widths []int) int {
	total := len(widths) - 1
	for _, w := range widths {
		total += w
	}
	return total
}

// shrinkWidths 把各行总宽从 width 之上收下来:先按各列「可让出的容量」等比分配剩余的宽度,
// 容量吃光(全部到下限)仍放不下时,再让末列独自继续收 —— 末列是命令/延迟这类自由文本,
// 收窄只丢它自己的信息,不会把整张表挤歪。
func shrinkWidths(natural, floors []int, width int) []int {
	n := len(natural)
	out := make([]int, n)
	capacity, over := 0, tableWidth(natural)-width
	for i := range natural {
		capacity += natural[i] - floors[i]
	}

	if over > capacity {
		copy(out, floors)
		for i := 0; i < over-capacity && out[n-1] > 1; i++ {
			out[n-1]--
		}
		return out
	}

	remaining, assigned := capacity-over, 0
	frac := make([]int, n)
	for i := range natural {
		extra := (natural[i] - floors[i]) * remaining / capacity
		out[i] = floors[i] + extra
		assigned += extra
		frac[i] = (natural[i] - floors[i]) * remaining % capacity
	}
	// 整数除法丢掉的零头按最大余数法分给余数最大的几列(一列至多一),否则整张表会比终端窄一截。
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return frac[order[a]] > frac[order[b]] })
	for i := 0; i < remaining-assigned; i++ {
		out[order[i]]++
	}
	return out
}

// renderTableRow 按既定列宽渲染一行。limit 是终端宽度,只用于最后那层硬裁:当各列下限之和
// (表头保底)本身就超过终端时,除了把整行截断没有别的办法保证不换行。
func renderTableRow(widths []int, cells []tableCell, limit int) (string, bool) {
	var b strings.Builder
	cut := false
	last := len(widths) - 1
	for j, w := range widths {
		if j > 0 {
			b.WriteByte(' ')
		}
		var cell tableCell
		if j < len(cells) {
			cell = cells[j]
		}
		s := cell.Full
		if cellWidth(s) > w {
			cut = true
			if cell.Compact != "" && cellWidth(cell.Compact) <= w {
				s = cell.Compact
			} else {
				s = truncateToWidth(s, w)
			}
		}
		b.WriteString(s)
		if j != last {
			if pad := w - cellWidth(s); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
	}
	line := b.String()
	if limit > 0 && cellWidth(line) > limit {
		return truncateToWidth(line, limit), true
	}
	return line, cut
}

// truncateToWidth 把字符串裁到不超过 width 列,末尾以 "..." 标记省略。按显示宽度裁(不是 rune
// 数):中文/全角一个字占两列,按 rune 数裁出来仍然会撑破终端宽度。
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if cellWidth(s) <= width {
		return s
	}
	if width <= cellWidth(ellipsis) {
		return strings.Repeat(".", width)
	}
	limit := width - cellWidth(ellipsis)
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + ellipsis
}

// cellWidth 是纯文本单元格在终端里占的列数。usage.go 里的 displayWidth 还要跳过 ANSI 样式
// 转义(那里的表格带颜色),表格只有裸文本,不需要为那套复杂度买单。
func cellWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 给出一个 rune 的终端列宽:东亚宽/全角字符占 2 列,组合符与零宽字符占 0 列。
// 这里只覆盖判定终端宽度必需的区间,不做完整的 Unicode EastAsianWidth 实现。
func runeWidth(r rune) int {
	switch {
	case r == 0 || r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == 0x200B, r == 0x200D, r == 0xFE0F:
		return 0
	}
	for _, rng := range wideRanges {
		if r >= rng[0] && r <= rng[1] {
			return 2
		}
	}
	return 1
}

// wideRanges 是东亚宽(East Asian Wide)与全角(Fullwidth)的主要区间,含码位区间与 emoji。
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2E80, 0x303E},   // CJK 部首、康熙部首、CJK 符号
	{0x3041, 0x33FF},   // 假名、注音、谚文兼容字母、CJK 笔画、带圈 CJK、CJK 兼容
	{0x3400, 0x4DBF},   // CJK 扩展 A
	{0x4E00, 0x9FFF},   // CJK 统一表意
	{0xA000, 0xA4CF},   // 彝文
	{0xAC00, 0xD7A3},   // 谚文音节
	{0xF900, 0xFAFF},   // CJK 兼容表意
	{0xFE10, 0xFE19},   // 竖排标点
	{0xFE30, 0xFE6F},   // CJK 兼容形式、小写变体
	{0xFF00, 0xFF60},   // 全角 ASCII、全角标点
	{0xFFE0, 0xFFE6},   // 全角符号
	{0x1F300, 0x1F64F}, // emoji(杂项符号、绘文字)
	{0x1F900, 0x1F9FF}, // 补充符号与绘文字
	{0x20000, 0x3FFFD}, // CJK 扩展 B 及以后
}

// termWidthOverride 是测试接缝:非 0 时覆盖终端宽度探测。
var termWidthOverride int

// outputWidth 返回 stdout 可用的列数,0 表示未知(未知即不省略)。
// 先问 TTY:TIOCGWINSZ 随窗口尺寸实时变化,是最可信的来源;再退到 COLUMNS,让非 TTY 场景
// (脚本、测试)也能人工钉住宽度。
func outputWidth() int {
	if termWidthOverride > 0 {
		return termWidthOverride
	}
	if w := termWidth(os.Stdout); w > 0 {
		return w
	}
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 0 {
		return w
	}
	return 0
}

// truncationHint 在表格确实省略过内容时提示完整内容的来源 —— 省略是为了不换行,但读者得知道
// 从哪里取回被省掉的部分。
func truncationHint(cmd string) string {
	return fmt.Sprintf("hint: 输出已按终端宽度省略;完整内容: %s --json", cmd)
}
