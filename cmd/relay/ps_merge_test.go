package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/portfwd"
	"github.com/user/relay/internal/relay/protocol"
)

// TestPortDisplay 覆盖 PORTS 单元格的渲染:回环绑定与历史的展示形态字节一致(R21 要求展示不回归),
// 非回环绑定额外前缀绑定面,让 0.0.0.0 这类发布面在 ps 里可见(KTD9)。规格一律经解析得到,
// 与真实链路同源。
func TestPortDisplay(t *testing.T) {
	parse := func(raw string) portfwd.Spec {
		t.Helper()
		spec, err := portfwd.Parse(raw, portfwd.DefaultBind)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return *spec
	}
	cases := []struct {
		name string
		spec portfwd.Spec
		want string
	}{
		// 回环绑定:与今天 String() 的展示形态逐字节相同。
		{"-p 8080", parse("8080"), "8080"},
		{"-p 8080:9000", parse("8080:9000"), "8080:9000"},
		{"-p 8080:db.local:5432", parse("8080:db.local:5432"), "8080:db.local:5432"},
		{"-p 127.0.0.1:8080", parse("127.0.0.1:8080"), "8080"},
		// 非回环绑定:绑定面现形,不再被展示悄悄收窄成回环。
		{"-p 0.0.0.0:8080", parse("0.0.0.0:8080"), "0.0.0.0:8080"},
		{"-p 0.0.0.0:8080:9000", parse("0.0.0.0:8080:9000"), "0.0.0.0:8080:9000"},
		// 非回环绑定 + 远端主机只能由 --bind-host 组合出来(单条 -p 字符串表达不了四段语义)。
		{
			"--bind-host 192.0.2.7 -p 8080:db.local:5432",
			portfwd.Spec{BindHost: "192.0.2.7", LocalPort: 8080, RemoteHost: "db.local", RemotePort: 5432},
			"192.0.2.7:8080:db.local:5432",
		},
		// "8080:8080" 与 "8080" 解析后是同一个规格,展示随之合并(记录已无损,只剩展示这一处差异)。
		{"-p 8080:8080", parse("8080:8080"), "8080"},
	}
	for _, c := range cases {
		if got := portDisplay(c.spec); got != c.want {
			t.Errorf("portDisplay(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTaskToPSRow 覆盖任务行投影的纯逻辑:运行中/已结束的 Exit、Cmd 截断、空 Node 兜底,
// 以及 PORTS 列的拼接与列宽裁剪。
func TestTaskToPSRow(t *testing.T) {
	now := time.Now().Unix()
	running := taskToPSRow(backend.TaskInfo{
		ID:        "t-1",
		Node:      "eg-node",
		Cmd:       "python -m http.server 8080",
		Ports:     []portfwd.Spec{{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000}},
		Status:    "running",
		ExitCode:  0,
		StartTime: now,
	})
	if running.Type != "task" {
		t.Errorf("Type = %q, want task", running.Type)
	}
	if running.ID != "t-1" || running.Node != "eg-node" || running.Status != "running" {
		t.Errorf("unexpected identity fields: %+v", running)
	}
	if running.Ports != "8080:9000" {
		t.Errorf("Ports = %q, want %q", running.Ports, "8080:9000")
	}
	if running.Exit != "-" {
		t.Errorf("running Exit = %q, want -", running.Exit)
	}
	if running.Command != "python -m http.server 8080" {
		t.Errorf("Command = %q", running.Command)
	}
	if !strings.HasSuffix(running.Created, "ago") {
		t.Errorf("Created = %q, want relative time", running.Created)
	}

	// 端口串保留完整值:裁剪只发生在表格渲染(见 TestPSLineTruncatesPortsNotCommand)。
	multi := taskToPSRow(backend.TaskInfo{ID: "t-4", Status: "running", StartTime: now, Ports: []portfwd.Spec{
		{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000},
		{BindHost: "127.0.0.1", LocalPort: 9090, RemoteHost: "127.0.0.1", RemotePort: 10000},
	}})
	if multi.Ports != "8080:9000,9090:10000" {
		t.Errorf("multi-port Ports = %q, want full value", multi.Ports)
	}

	// 非回环绑定:展示形态额外带上绑定面,让 0.0.0.0 这类发布面在 ps 里可见(R21/KTD9)。
	wildcard := taskToPSRow(backend.TaskInfo{ID: "t-5", Status: "running", StartTime: now, Ports: []portfwd.Spec{
		{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080},
	}})
	if wildcard.Ports != "0.0.0.0:8080" {
		t.Errorf("non-loopback Ports = %q, want %q", wildcard.Ports, "0.0.0.0:8080")
	}

	// 旧记录的展示字符串形态解出来之后与结构化形态同源:回环绑定渲染与今天一致。
	legacy := taskToPSRow(backend.TaskInfo{ID: "t-6", Status: "running", StartTime: now, Ports: legacyPorts(t, `["8080:9000"]`)})
	if legacy.Ports != "8080:9000" {
		t.Errorf("legacy Ports = %q, want %q", legacy.Ports, "8080:9000")
	}

	// Cmd 保留完整命令行:长命令在表格里也照原样展示(COMMAND 是末列,裁剪只丢信息)。
	longCmd := strings.Repeat("a", 32) + "bbbbbbbbbb"
	finished := taskToPSRow(backend.TaskInfo{
		ID:        "t-2",
		Cmd:       longCmd,
		Status:    "exited",
		ExitCode:  137,
		StartTime: now,
	})
	if finished.Exit != "137" {
		t.Errorf("finished Exit = %q, want 137", finished.Exit)
	}
	if finished.Node != "(unknown)" {
		t.Errorf("empty Node = %q, want (unknown)", finished.Node)
	}
	if finished.Command != longCmd {
		t.Errorf("Command = %q, want the full command line", finished.Command)
	}

	// 无端口 → "-"
	noPorts := taskToPSRow(backend.TaskInfo{ID: "t-3", Status: "running", StartTime: now})
	if noPorts.Ports != "-" {
		t.Errorf("no ports = %q, want -", noPorts.Ports)
	}

	// R20:任务行显示 restart 策略(复用 psRow.Restart 字段)。
	polRow := taskToPSRow(backend.TaskInfo{ID: "t-7", Node: "eg-node", Status: "running", StartTime: now, Restart: "always"})
	if polRow.Restart != "always" {
		t.Errorf("task Restart = %q, want always", polRow.Restart)
	}

	// R12:CREATED 取首次创建时间,不因重启而重置;老执行方 CreatedAt=0 时回退到最近一次启动时间。
	createdAt := now - 3600 // 1 小时前创建,最近一次启动在 1 分钟前
	freshStart := taskToPSRow(backend.TaskInfo{ID: "t-8", Status: "running", StartTime: now, CreatedAt: createdAt})
	if !strings.Contains(freshStart.Created, "h ago") {
		t.Errorf("CREATED with CreatedAt = %q, want ~1h ago", freshStart.Created)
	}
	createdReset := taskToPSRow(backend.TaskInfo{ID: "t-9", Status: "running", StartTime: now})
	if !strings.Contains(createdReset.Created, "s ago") {
		t.Errorf("CREATED fallback (CreatedAt=0) = %q, want ~now", createdReset.Created)
	}
}

// TestPSRowKeepsFullCommand 覆盖“命令太长看不全”的回归:行模型(即 --json 的字段来源)保留完整
// 命令行,表格也不裁 COMMAND —— 它是末列,收窄不影响对齐、只会丢信息。定宽 PORTS 列照旧收窄。
func TestPSRowKeepsFullCommand(t *testing.T) {
	longCmd := "python3 -m uvicorn app.main:app --host 0.0.0.0 --port 8000 --workers 4"
	row := taskToPSRow(backend.TaskInfo{
		ID:        "t-1",
		Node:      "eg-node",
		Status:    "running",
		StartTime: time.Now().Unix(),
		Cmd:       longCmd,
		Ports: []portfwd.Spec{
			{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000},
			{BindHost: "127.0.0.1", LocalPort: 9090, RemoteHost: "127.0.0.1", RemotePort: 10000},
		},
	})

	line := psLine(row)
	if !strings.HasSuffix(line, longCmd+"\n") {
		t.Errorf("table line = %q, want it to end with the full command %q", line, longCmd)
	}
	if !strings.Contains(line, "8080:9000,909...") {
		t.Errorf("table line = %q, want the PORTS cell narrowed to %d chars", line, psPortWidth)
	}

	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var got struct {
		Command string `json:"command"`
		Ports   string `json:"ports"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if got.Command != longCmd {
		t.Errorf("json command = %q, want %q", got.Command, longCmd)
	}
	if got.Ports != "8080:9000,9090:10000" {
		t.Errorf("json ports = %q, want the full port list", got.Ports)
	}
}

// TestFitCell 覆盖单元格收窄:够宽原样返回,超宽按 rune 裁剪(Windows 主机名可以是 Unicode,
// 按字节裁会切出非法 UTF-8)。
func TestFitCell(t *testing.T) {
	if got := fitCell("8080:9000", psPortWidth); got != "8080:9000" {
		t.Errorf("fitCell(short) = %q, want unchanged", got)
	}
	if got := fitCell("8080:9000,9090:10000", psPortWidth); got != "8080:9000,909..." {
		t.Errorf("fitCell(long) = %q, want %q", got, "8080:9000,909...")
	}
	got := fitCell(strings.Repeat("中文主机", 5)+":8080", psPortWidth)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != psPortWidth {
		t.Errorf("fitCell(unicode) = %q, want %d valid runes", got, psPortWidth)
	}
}

// TestPSHeaderContainsRestart 钉住 R20 的 RESTART 列:表头与行共用 psRowFormat,列必须出现在 PORTS
// 与 EXIT 之间,否则对齐会错位。
func TestPSHeaderContainsRestart(t *testing.T) {
	h := psHeader()
	if !strings.Contains(h, "RESTART") {
		t.Fatalf("ps header %q missing RESTART column", h)
	}
	if !strings.Contains(h, "PORTS") || !strings.Contains(h, "EXIT") {
		t.Fatalf("ps header %q missing PORTS or EXIT", h)
	}
	if strings.Index(h, "PORTS") > strings.Index(h, "RESTART") || strings.Index(h, "RESTART") > strings.Index(h, "EXIT") {
		t.Fatalf("ps header %q: RESTART must sit between PORTS and EXIT", h)
	}
}

// legacyPorts 把升级前落盘的展示字符串形态(["8080", ...])按 wire 反序列化:
// 老执行方上行的简报与老 meta.json 都是这个形态,CLI 必须解得开(R21 的兼容要求)。
func legacyPorts(t *testing.T, raw string) []portfwd.Spec {
	t.Helper()
	var envelope struct {
		Ports []portfwd.Spec `json:"ports"`
	}
	if err := json.Unmarshal([]byte(`{"ports":`+raw+`}`), &envelope); err != nil {
		t.Fatalf("unmarshal legacy ports %s: %v", raw, err)
	}
	return envelope.Ports
}

// TestTunnelToPSRow 覆盖隧道行投影:running/stopped、零值 Started → Created "-"。
func TestTunnelToPSRow(t *testing.T) {
	started := time.Now().Add(-3 * time.Minute)
	row := tunnelToPSRow(tunnelState{
		Egress:  "eg-node",
		Listen:  "127.0.0.1:1080",
		Restart: tunRestartAlways,
		State:   "running",
		Started: started,
	})
	if row.Type != "tunnel" {
		t.Errorf("Type = %q, want tunnel", row.Type)
	}
	if row.ID != "eg-node" || row.Node != "eg-node" || row.Egress != "eg-node" {
		t.Errorf("unexpected identity fields: %+v", row)
	}
	if row.Status != "running" || row.Ports != "127.0.0.1:1080" {
		t.Errorf("unexpected status/ports: %+v", row)
	}
	if row.Exit != "-" {
		t.Errorf("Exit = %q, want -", row.Exit)
	}
	if row.Command != "egress eg-node" {
		t.Errorf("Command = %q, want %q", row.Command, "egress eg-node")
	}
	if row.Restart != tunRestartAlways {
		t.Errorf("Restart = %q", row.Restart)
	}
	if !strings.HasSuffix(row.Created, "ago") {
		t.Errorf("Created = %q, want relative time", row.Created)
	}

	stopped := tunnelToPSRow(tunnelState{Egress: "eg-2", Listen: "127.0.0.1:1081", State: "stopped"})
	if stopped.Created != "-" {
		t.Errorf("zero Started Created = %q, want -", stopped.Created)
	}
	if stopped.Status != "stopped" {
		t.Errorf("Status = %q, want stopped", stopped.Status)
	}
}

// TestTunnelLogLineMatches 覆盖前缀匹配:必须连右括号一起匹配,防止 eg-node 误吞 eg-node-2。
func TestTunnelLogLineMatches(t *testing.T) {
	cases := []struct {
		line   string
		egress string
		want   bool
	}{
		{"[tunnel eg-node] restored (running)", "eg-node", true},
		{"2026/10/12 10:00:00 [tunnel eg-node] 1.2.3.4:80 egress eg-node: dial timeout", "eg-node", true},
		{"[tunnel eg-other] restored (running)", "eg-node", false},
		{"[tunnel eg-node-2] restored (running)", "eg-node", false},
		{"[Node] Tunnel holder listening", "eg-node", false},
		{"", "eg-node", false},
	}
	for _, c := range cases {
		if got := tunnelLogLineMatches(c.line, c.egress); got != c.want {
			t.Errorf("tunnelLogLineMatches(%q, %q) = %v, want %v", c.line, c.egress, got, c.want)
		}
	}
}

// TestEgressStateOf 覆盖出口就绪投影:离线看 LastSeen、Seg2 不可用=notready、其余 ready + 紧凑延迟。
// 判定必须与 `relay nodes` 一致,否则同一台 executor 在两条命令里会给出相反结论。
func TestEgressStateOf(t *testing.T) {
	ms := func(v int64) *int64 { return &v }

	cases := []struct {
		name string
		in   protocol.ExecutorHealth
		want egressState
	}{
		{
			name: "offline: 断线且带 LastSeen",
			in:   protocol.ExecutorHealth{ExecutorID: "eg", Online: false, LastSeen: 1700000000},
			want: egressState{Text: "offline"},
		},
		{
			name: "ready: 在线(旧中转身不返回 Online/LastSeen)",
			in:   protocol.ExecutorHealth{ExecutorID: "eg"},
			want: egressState{Ready: true, Text: "ready"},
		},
		{
			name: "notready: Seg2 不可用",
			in: protocol.ExecutorHealth{
				ExecutorID: "eg",
				Seg2:       protocol.StatusSegment{Unavailable: "executor offline"},
			},
			want: egressState{Text: "notready"},
		},
		{
			name: "ready: 带端到端延迟",
			in: protocol.ExecutorHealth{
				ExecutorID: "eg",
				Total:      protocol.StatusSegment{LatencyMS: ms(11)},
			},
			want: egressState{Ready: true, Text: "ready", Latency: "11ms"},
		},
	}
	for _, c := range cases {
		if got := egressStateOf(c.in); got != c.want {
			t.Errorf("%s: egressStateOf = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestApplyEgressStatus 覆盖出口就绪落到隧道行:表格 STATUS 文案(displayStatus)与 JSON 字段;
// JSON 的 status 必须保持纯状态值,不能被人类文案污染。stopped 隧道与任务行不受影响。
func TestApplyEgressStatus(t *testing.T) {
	ready := map[string]egressState{"eg": {Ready: true, Text: "ready", Latency: "11ms"}}
	cases := []struct {
		name        string
		row         psRow
		m           map[string]egressState
		wantDisplay string
		wantText    string
		wantReady   *bool
	}{
		{
			name:        "ready",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           ready,
			wantDisplay: "running (egress ready 11ms)",
			wantText:    "ready",
			wantReady:   boolPtr(true),
		},
		{
			name:        "ready 无延迟",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           map[string]egressState{"eg": {Ready: true, Text: "ready"}},
			wantDisplay: "running (egress ready)",
			wantText:    "ready",
			wantReady:   boolPtr(true),
		},
		{
			name:        "offline",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           map[string]egressState{"eg": {Text: "offline"}},
			wantDisplay: "running (egress offline)",
			wantText:    "offline",
			wantReady:   boolPtr(false),
		},
		{
			name:        "notready",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           map[string]egressState{"eg": {Text: "notready"}},
			wantDisplay: "running (egress notready)",
			wantText:    "notready",
			wantReady:   boolPtr(false),
		},
		{
			name:        "探测不可用(m 为 nil) → unknown",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           nil,
			wantDisplay: "running (egress unknown)",
			wantText:    "unknown",
		},
		{
			name:        "台账里没有该 egress → unknown",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "running"},
			m:           map[string]egressState{"other": {Ready: true, Text: "ready"}},
			wantDisplay: "running (egress unknown)",
			wantText:    "unknown",
		},
		{
			name:        "stopped 隧道不动",
			row:         psRow{Type: "tunnel", Egress: "eg", Status: "stopped"},
			m:           ready,
			wantDisplay: "stopped",
		},
		{
			name:        "任务行不动",
			row:         psRow{Type: "task", Egress: "eg", Status: "running"},
			m:           ready,
			wantDisplay: "running",
		},
	}
	for _, c := range cases {
		r := c.row
		applyEgressStatus(&r, c.m)
		// JSON 的 status 永不改变。
		if r.Status != c.row.Status {
			t.Errorf("%s: Status = %q, want unchanged %q", c.name, r.Status, c.row.Status)
		}
		if got := r.displayStatus(); got != c.wantDisplay {
			t.Errorf("%s: displayStatus = %q, want %q", c.name, got, c.wantDisplay)
		}
		if r.EgressStatus != c.wantText {
			t.Errorf("%s: EgressStatus = %q, want %q", c.name, r.EgressStatus, c.wantText)
		}
		switch {
		case c.wantReady == nil && r.EgressReady != nil:
			t.Errorf("%s: EgressReady = %v, want nil", c.name, *r.EgressReady)
		case c.wantReady != nil && r.EgressReady == nil:
			t.Errorf("%s: EgressReady = nil, want %v", c.name, *c.wantReady)
		case c.wantReady != nil && r.EgressReady != nil && *r.EgressReady != *c.wantReady:
			t.Errorf("%s: EgressReady = %v, want %v", c.name, *r.EgressReady, *c.wantReady)
		}
	}

	// displayStatus 未叠加时空回落到 status(任务行/表格历史行为)。
	if got := (psRow{Status: "exited"}).displayStatus(); got != "exited" {
		t.Errorf("no-overlay displayStatus = %q, want exited", got)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestFilterTunnels 覆盖隧道过滤:未加 -a 只留 running;target 按 egress 精确过滤。
func TestFilterTunnels(t *testing.T) {
	states := []tunnelState{
		{Egress: "eg-a", State: "running"},
		{Egress: "eg-b", State: "stopped"},
		{Egress: "eg-c", State: "running"},
	}

	if got := filterTunnels(states, "", false); len(got) != 2 || got[0].Egress != "eg-a" || got[1].Egress != "eg-c" {
		t.Errorf("without -a = %+v, want only running eg-a/eg-c", got)
	}
	if got := filterTunnels(states, "", true); len(got) != 3 {
		t.Errorf("with -a = %+v, want all 3", got)
	}
	if got := filterTunnels(states, "eg-b", false); len(got) != 0 {
		t.Errorf("target stopped without -a = %+v, want empty", got)
	}
	if got := filterTunnels(states, "eg-b", true); len(got) != 1 || got[0].Egress != "eg-b" {
		t.Errorf("target stopped with -a = %+v, want [eg-b]", got)
	}
	if got := filterTunnels(nil, "", false); len(got) != 0 {
		t.Errorf("nil input = %+v, want empty", got)
	}
}

// TestReadTunnelLogTail 覆盖 node.log 的有界尾部读取:末 N 行、窗口不足以命中时翻倍扩张、缺失文件报错。
func TestReadTunnelLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "node.log")

	// 1. 交错命中:tail 应取最后 3 条属于 eg-x 的行。
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "noise line %d\n", i)
		if i%50 == 0 {
			fmt.Fprintf(&b, "[tunnel eg-x] event %d\n", i)
		}
	}
	fmt.Fprintf(&b, "[tunnel eg-other] should-not-match\n")
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	got, err := readTunnelLogTail(logPath, "eg-x", 3)
	if err != nil {
		t.Fatalf("readTunnelLogTail: %v", err)
	}
	want := []string{"[tunnel eg-x] event 50", "[tunnel eg-x] event 100", "[tunnel eg-x] event 150"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("tail 3 = %v, want %v", got, want)
	}

	// 2. 命中集中在文件头部,后面是大段噪声(> 64KB 初始窗口):必须靠窗口翻倍才能读到。
	var big strings.Builder
	big.WriteString("[tunnel eg-y] first\n")
	big.WriteString("[tunnel eg-y] second\n")
	big.WriteString("[tunnel eg-y] third\n")
	for i := 0; i < 4000; i++ {
		big.WriteString("padding padding padding padding padding padding padding\n")
	}
	bigPath := filepath.Join(dir, "big.log")
	if err := os.WriteFile(bigPath, []byte(big.String()), 0o644); err != nil {
		t.Fatalf("write big log: %v", err)
	}
	if fi, _ := os.Stat(bigPath); fi.Size() <= 64*1024 {
		t.Fatalf("big log too small (%d bytes) to exercise window growth", fi.Size())
	}
	gotBig, err := readTunnelLogTail(bigPath, "eg-y", 2)
	if err != nil {
		t.Fatalf("readTunnelLogTail big: %v", err)
	}
	if len(gotBig) != 2 || gotBig[0] != "[tunnel eg-y] second" || gotBig[1] != "[tunnel eg-y] third" {
		t.Fatalf("big tail 2 = %v, want last two eg-y lines", gotBig)
	}

	// 3. 缺失文件 → 返回 os.IsNotExist 可识别的错误(供 showTunnelLogs 决定是否补提示)。
	if _, err := readTunnelLogTail(filepath.Join(dir, "absent.log"), "eg-x", 3); !os.IsNotExist(err) {
		t.Fatalf("missing file err = %v, want IsNotExist", err)
	}
}
