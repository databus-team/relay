package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/protocol"
)

// TestTaskToPSRow 覆盖任务行投影的纯逻辑:运行中/已结束的 Exit、Cmd 截断、空 Node 兜底。
func TestTaskToPSRow(t *testing.T) {
	now := time.Now().Unix()
	running := taskToPSRow(backend.TaskInfo{
		ID:        "t-1",
		Node:      "eg-node",
		Cmd:       "python -m http.server 8080",
		Ports:     []string{"8080:8080"},
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
	if running.Ports != "8080:8080" {
		t.Errorf("Ports = %q", running.Ports)
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

	// 端口串 >16 → 前 13 + "..."(复刻旧 runPs 的列宽裁剪)
	multi := taskToPSRow(backend.TaskInfo{ID: "t-4", Status: "running", StartTime: now, Ports: []string{"8080:8080", "9090:9090"}})
	if multi.Ports != "8080:8080,909..." {
		t.Errorf("multi-port Ports = %q, want truncated", multi.Ports)
	}

	// Cmd 截断:>35 → 前 32 + "..."(复刻旧 runPs 行为)
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
	wantCmd := longCmd[:32] + "..."
	if finished.Command != wantCmd {
		t.Errorf("truncated Command = %q, want %q", finished.Command, wantCmd)
	}
	if len(finished.Command) != 35 {
		t.Errorf("truncated Command len = %d, want 35", len(finished.Command))
	}

	// 无端口 → "-"
	noPorts := taskToPSRow(backend.TaskInfo{ID: "t-3", Status: "running", StartTime: now})
	if noPorts.Ports != "-" {
		t.Errorf("no ports = %q, want -", noPorts.Ports)
	}
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
