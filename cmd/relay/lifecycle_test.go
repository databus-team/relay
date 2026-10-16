package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/portfwd"
)

// fakeTaskBackend 是实现 TaskSupervisorBackend 的假后端,记录 TaskRestart/TaskStop 调用以供断言
// R2 的「不重启」与 R9 的「同 id 重跑」。其余方法空实现。
type fakeTaskBackend struct {
	tasks      []backend.TaskInfo
	restarts   []string
	stops      []string
	restartErr error
	stopErr    error
}

func (f *fakeTaskBackend) TaskList(ctx context.Context, targetExecutor string, all bool) ([]backend.TaskInfo, error) {
	if targetExecutor != "" {
		var out []backend.TaskInfo
		for _, t := range f.tasks {
			if t.Node == targetExecutor {
				out = append(out, t)
			}
		}
		return out, nil
	}
	return f.tasks, nil
}

func (f *fakeTaskBackend) TaskStop(ctx context.Context, targetExecutor, taskID string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stops = append(f.stops, taskID)
	return nil
}

func (f *fakeTaskBackend) TaskRestart(ctx context.Context, targetExecutor, taskID string) error {
	if f.restartErr != nil {
		return f.restartErr
	}
	f.restarts = append(f.restarts, taskID)
	return nil
}

func (f *fakeTaskBackend) TaskLogs(ctx context.Context, targetExecutor, taskID string, follow bool, tailLines int, onChunk func(backend.ExecChunk)) error {
	return nil
}

// TestResolveLifecycle 覆盖名字解析的各分支(R1/R3/R4/KTD8):任务清单(含已停止)优先,再本机
// holder,最后意图文件;同名同时命中任务与隧道时任务优先;「查不到」被区分为不在 vs 不可达。
func TestResolveLifecycle(t *testing.T) {
	task := backend.TaskInfo{
		ID:     "t-shared",
		Node:   "eg-a",
		Status: taskStatusRunning,
		Ports:  []portfwd.Spec{{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080}},
	}
	holderTun := tunnelState{Egress: "eg-b", Listen: "127.0.0.1:1080", Restart: tunRestartAlways, State: "running"}
	intentTun := tunnelState{Egress: "eg-c", Listen: "0.0.0.0:1081", Restart: tunRestartNo, State: "running"}

	noTask := func() ([]backend.TaskInfo, error) { return nil, nil }
	noTun := func() ([]tunnelState, error) { return nil, nil }

	cases := []struct {
		name            string
		targetName      string // 要解析的名字(缺省用任务 id / 隧道 egress)
		deps            lifecycleDeps
		wantKind        lifecycleKind
		wantNode        string
		wantTask        bool
		wantTunnel      bool
		wantForward     bool
		wantNodeRunning bool
		wantUnreach     bool
	}{
		{
			name:     "任务命中(含已停止,all=true)",
			deps:     lifecycleDeps{NodeRunning: true, ListTasks: func() ([]backend.TaskInfo, error) { return []backend.TaskInfo{task}, nil }, Tunnels: noTun},
			wantKind: lifecycleKindTask, wantNode: "eg-a", wantTask: true,
		},
		{
			name:       "holder 隧道命中",
			targetName: "eg-b",
			deps:       lifecycleDeps{NodeRunning: true, ListTasks: noTask, Tunnels: func() ([]tunnelState, error) { return []tunnelState{holderTun, intentTun}, nil }},
			wantKind:   lifecycleKindTunnel, wantNode: "eg-b", wantTunnel: true, wantNodeRunning: true,
		},
		{
			name:       "意图文件隧道命中(node 未运行)",
			targetName: "eg-c",
			deps:       lifecycleDeps{NodeRunning: false, ListTasks: noTask, Tunnels: func() ([]tunnelState, error) { return []tunnelState{intentTun}, nil }},
			wantKind:   lifecycleKindTunnel, wantNode: "eg-c", wantTunnel: true, wantNodeRunning: false,
		},
		{
			name: "同名同时命中任务与隧道 → 任务优先(R3)",
			deps: lifecycleDeps{NodeRunning: true, ListTasks: func() ([]backend.TaskInfo, error) { return []backend.TaskInfo{task}, nil },
				Tunnels: func() ([]tunnelState, error) {
					return []tunnelState{{Egress: task.ID, Listen: "127.0.0.1:9", State: "stopped"}}, nil
				}},
			wantKind: lifecycleKindTask, wantNode: "eg-a", wantTask: true,
		},
		{
			name:     "任务与隧道都查不到 → 未知名",
			deps:     lifecycleDeps{NodeRunning: true, ListTasks: noTask, Tunnels: noTun},
			wantKind: lifecycleKindUnknown,
		},
		{
			name:     "清单问不到(执行方离线/中转不可达)且无隧道",
			deps:     lifecycleDeps{NodeRunning: true, ListTasks: func() ([]backend.TaskInfo, error) { return nil, errors.New("connect refused") }, Tunnels: noTun},
			wantKind: lifecycleKindUnknown, wantUnreach: true,
		},
		{
			name:       "清单问不到但隧道仍命中(R4 隧道侧降级)",
			targetName: "eg-b",
			deps:       lifecycleDeps{NodeRunning: true, ListTasks: func() ([]backend.TaskInfo, error) { return nil, errors.New("connect refused") }, Tunnels: func() ([]tunnelState, error) { return []tunnelState{holderTun}, nil }},
			wantKind:   lifecycleKindTunnel, wantNode: "eg-b", wantTunnel: true, wantNodeRunning: true,
		},
		{
			name:     "-e/--executor 限定执行方内命中",
			deps:     lifecycleDeps{Target: "eg-a", NodeRunning: true, ListTasks: func() ([]backend.TaskInfo, error) { return []backend.TaskInfo{task}, nil }, Tunnels: noTun},
			wantKind: lifecycleKindTask, wantNode: "eg-a", wantTask: true,
		},
		{
			name:       "独立转发生命(端口为名)",
			targetName: "7001",
			deps: lifecycleDeps{NodeRunning: true, ListTasks: noTask, Tunnels: noTun,
				Forwards: func() ([]forwardState, error) {
					return []forwardState{{Port: 7001, Executor: "eg-f", State: "running"}}, nil
				}},
			wantKind: lifecycleKindForward, wantNode: "eg-f", wantForward: true, wantNodeRunning: true,
		},
		{
			name:       "任务/隧道都无,转发命中(node 未运行也可解析)",
			targetName: "7001",
			deps: lifecycleDeps{NodeRunning: false, ListTasks: noTask, Tunnels: noTun,
				Forwards: func() ([]forwardState, error) {
					return []forwardState{{Port: 7001, Executor: "eg-f", State: "stopped"}}, nil
				}},
			wantKind: lifecycleKindForward, wantNode: "eg-f", wantForward: true, wantNodeRunning: false,
		},
		{
			name:       "三向同名(任务 id == 隧道 egress == 转发端口)仍任务优先(R3)",
			targetName: "7001",
			deps: lifecycleDeps{NodeRunning: true,
				ListTasks: func() ([]backend.TaskInfo, error) {
					return []backend.TaskInfo{{ID: "7001", Node: "eg-task", Status: taskStatusRunning}}, nil
				},
				Tunnels: func() ([]tunnelState, error) {
					return []tunnelState{{Egress: "7001", Listen: "127.0.0.1:9", State: "stopped"}}, nil
				},
				Forwards: func() ([]forwardState, error) {
					return []forwardState{{Port: 7001, Executor: "eg-f", State: "running"}}, nil
				}},
			wantKind: lifecycleKindTask, wantNode: "eg-task",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := c.targetName
			if name == "" {
				name = "t-shared"
			}
			got := resolveLifecycle(name, c.deps)
			if got.Kind != c.wantKind {
				t.Fatalf("Kind = %v, want %v", got.Kind, c.wantKind)
			}
			if c.wantTask && !(got.Kind == lifecycleKindTask && got.Task.ID == "t-shared") {
				t.Errorf("want task t-shared, got %+v", got)
			}
			if c.wantTunnel && !(got.Kind == lifecycleKindTunnel && got.State.Egress == c.wantNode) {
				t.Errorf("want tunnel egress %s, got %+v", c.wantNode, got)
			}
			if c.wantForward && !(got.Kind == lifecycleKindForward && got.Forward.Port == 7001) {
				t.Errorf("want forward port 7001, got %+v", got)
			}
			if c.wantKind == lifecycleKindTunnel && got.NodeRunning != c.wantNodeRunning {
				t.Errorf("NodeRunning = %v, want %v", got.NodeRunning, c.wantNodeRunning)
			}
			if c.wantKind == lifecycleKindForward && got.NodeRunning != c.wantNodeRunning {
				t.Errorf("forward NodeRunning = %v, want %v", got.NodeRunning, c.wantNodeRunning)
			}
			if c.wantUnreach && got.Unreachable == "" {
				t.Errorf("expected Unreachable set, got empty")
			}
			if !c.wantUnreach && got.Unreachable != "" {
				t.Errorf("Unreachable = %q, want empty", got.Unreachable)
			}
		})
	}
}

// TestLifecycleUnresolvedError 覆盖 R1/R4 的三类文案:名字不存在给出创建方式(R1);执行方离线/中转
// 不可达时说明可能离线、可用 -e 指定且不提示重建(R4);带 -e 限定执行方时错误里带它的 id(R4)。
func TestLifecycleUnresolvedError(t *testing.T) {
	cases := []struct {
		name    string
		target  lifecycleTarget
		wantSub []string // 必须全部命中
		noSub   []string // 必须全部不出现
	}{
		{
			name:    "未知名:给出创建方式(任务与隧道两条路径)(R1)",
			target:  lifecycleTarget{Kind: lifecycleKindUnknown},
			wantSub: []string{"relay run", "relay run -t tunnel"},
			noSub:   []string{"不存在"},
		},
		{
			name:    "执行方离线/中转不可达:可能离线、用 -e,不提示重建(R4)",
			target:  lifecycleTarget{Kind: lifecycleKindUnknown, Unreachable: "no executor registered"},
			wantSub: []string{"may be offline", "-e/--executor"},
			noSub:   []string{"relay run", "exists"},
		},
		{
			name:    "带 -e 限定执行方:错误里带它的 executor id(R4)",
			target:  lifecycleTarget{Kind: lifecycleKindUnknown, Target: "eg-a"},
			wantSub: []string{"eg-a"},
		},
		{
			name:    "带 -e 但执行方问不到:带 id + 可能离线(R4)",
			target:  lifecycleTarget{Kind: lifecycleKindUnknown, Target: "eg-a", Unreachable: "no executor registered"},
			wantSub: []string{"eg-a", "may be offline"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := lifecycleUnresolvedError("some-name", c.target)
			for _, s := range c.wantSub {
				if !strings.Contains(msg, s) {
					t.Errorf("%q missing %q", msg, s)
				}
			}
			for _, s := range c.noSub {
				if strings.Contains(msg, s) {
					t.Errorf("%q must not contain %q", msg, s)
				}
			}
		})
	}
}

// TestApplyTaskLifecycle 覆盖 R2(已在运行的任务 start 不重启)与 R9(否则下发同 id 重启),以及
// R13 所需的端口规格始终随结果返回(供调用方按原绑定面重登记)。
func TestApplyTaskLifecycle(t *testing.T) {
	ports := []portfwd.Spec{{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080}}

	cases := []struct {
		name        string
		verb        lifecycleVerb
		status      string
		wantRestart bool
		wantErr     bool
	}{
		{name: "start 已停止任务 → 重启", verb: verbStart, status: "stopped", wantRestart: true},
		{name: "start 运行中任务 → 不重启(R2)", verb: verbStart, status: taskStatusRunning, wantRestart: false},
		{name: "restart 运行中任务 → 重启(R9/R11)", verb: verbRestart, status: taskStatusRunning, wantRestart: true},
		{name: "restart 已停止任务 → 重启", verb: verbRestart, status: "exited", wantRestart: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb := &fakeTaskBackend{}
			var err error
			var restarted bool
			var got []portfwd.Spec
			for attempt := 0; attempt < 2; attempt++ {
				task := backend.TaskInfo{ID: "t-1", Node: "eg-a", Status: c.status, Ports: ports}
				got, restarted, err = applyTaskLifecycle(c.verb, fb, task.Node, task)
				if err != nil {
					break
				}
				if c.wantRestart {
					if !restarted || len(fb.restarts) != 1 || fb.restarts[0] != "t-1" {
						t.Fatalf("want restart t-1, restarts=%v restarted=%v", fb.restarts, restarted)
					}
				} else {
					if restarted {
						t.Fatal("R2: start on running task must not restart")
					}
				}
				// 端口规格必须随结果返回,供 R13 按原绑定面重登记(R21)。
				if len(got) != 1 || got[0].BindHost != "0.0.0.0" {
					t.Fatalf("returned ports = %+v, want bind 0.0.0.0", got)
				}
				break
			}
			if c.wantErr && err == nil {
				t.Fatal("want error")
			}
		})
	}

	// 重启失败:错误里带该任务的执行方 id(R4 任务侧)。
	fb := &fakeTaskBackend{restartErr: errors.New("boom")}
	_, _, err := applyTaskLifecycle(verbRestart, fb, "eg-a", backend.TaskInfo{ID: "t-1", Node: "eg-a", Status: "stopped"})
	if err == nil || !strings.Contains(err.Error(), "eg-a") {
		t.Errorf("restart error = %v, want executor id eg-a", err)
	}
}

// TestStopLocalTunnel_NodeDownRemovesIntent 覆盖 R18:node 未运行时 stop 直接移除意图(否则
// always 条目会在 node 下次启动被 restore 重放);对已忘掉的对象再停一次不报错(R2 幂等)。
func TestStopLocalTunnel_NodeDownRemovesIntent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	if err := persistTunnels(statePath, []tunnelState{
		{Egress: "eg-a", Listen: "127.0.0.1:1080", Restart: tunRestartAlways, State: "running"},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	out, err := stopLocalTunnel(statePath, "", "eg-a", false)
	if err != nil || !stopSucceeded(out, err) || out != tunnelStopRemoved {
		t.Fatalf("stop = (%v, %v), want removed", out, err)
	}
	states, _ := loadTunnels(statePath)
	if len(states) != 0 {
		t.Fatalf("intent not removed: %+v", states)
	}

	// 已忘掉:再停一次不报错,如实说「没有可停的」。
	out, err = stopLocalTunnel(statePath, "", "eg-a", false)
	if out != tunnelStopAbsent || err != nil {
		t.Fatalf("re-stop = (%v, %v), want absent", out, err)
	}

	// 从不在意图文件里。
	if out, err = stopLocalTunnel(statePath, "", "eg-zz", false); out != tunnelStopAbsent || err != nil {
		t.Fatalf("never-present = (%v, %v), want absent", out, err)
	}
}

// TestStopLocalTunnel_NodeRunningClosesViaHolder 覆盖 node 运行时的停止:经 holder 关闭并移除意图,
// holder 里不再列出该 egress。
func TestStopLocalTunnel_NodeRunningClosesViaHolder(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	_, _, sock := startTestHolder(t, statePath, io.Discard)
	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	if err := openTunnelViaHolder(sock, "eg-x", listen, tunRestartAlways, io.Discard); err != nil {
		t.Fatalf("open: %v", err)
	}

	out, err := stopLocalTunnel(statePath, sock, "eg-x", true)
	if err != nil || out != tunnelStopClosed {
		t.Fatalf("stop = (%v, %v), want closed", out, err)
	}
	time.Sleep(20 * time.Millisecond) // 监听关闭在 holder 侧异步,给一点时间
	resp, _ := sendTunnelControl(sock, tunControlMsg{Op: tunOpList})
	for _, st := range resp.Tunnels {
		if st.Egress == "eg-x" {
			t.Errorf("eg-x still listed after stop: %+v", st)
		}
	}
}

// TestTunnelStatesForRead_NodeDownReadsIntentAsStopped 覆盖 R19:`relay ps -a` 在 node 未运行时仍
// 列出意图文件里的隧道,但 state 必须如实标成 stopped(没有任何进程持有监听,文件残留的 running 不再成立)。
func TestTunnelStatesForRead_NodeDownReadsIntentAsStopped(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	if err := persistTunnels(statePath, []tunnelState{
		{Egress: "eg-a", Listen: "127.0.0.1:1080", Restart: tunRestartAlways, State: "running"},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	states, err := tunnelStatesForRead(statePath, "", false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(states) != 1 || states[0].Egress != "eg-a" || states[0].State != "stopped" {
		t.Fatalf("states = %+v, want [eg-a stopped]", states)
	}
	// `relay ps -a`(不过滤)应列出它:filterTunnels all=true 保留所有。
	if got := filterTunnels(states, "", true); len(got) != 1 {
		t.Errorf("ps -a = %+v, want 1 row", got)
	}
	// 缺失文件 → 空集(不视为错误)。
	if states, err = tunnelStatesForRead(filepath.Join(t.TempDir(), "absent.json"), "", false); err != nil || len(states) != 0 {
		t.Errorf("absent = (%+v, %v), want empty", states, err)
	}
}

// startTestForwardHolder 在临时目录启动一个端口转发持有者(与 forward_holder_test 同构),
// 等待 unix 控制 socket 就绪后返回 sockPath 与 holder。rb 用 nil:register/listen 不触网
// (pump 只在连接被 accept 后才需要后端,这些用例不建立连接),故只验证转发规则的登记与绑定面
// —— 完整透传连通性由 forward_holder_test 的 E2E 覆盖。
func startTestForwardHolder(t *testing.T) (string, *forwardHolder) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	holder := newForwardHolder(ctx, nil, sockPath)
	holderErr := make(chan error, 1)
	go func() { holderErr <- holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-holderErr:
			t.Fatalf("holder.Run: %v", err)
		default:
		}
		if _, err := os.Stat(sockPath); err == nil {
			return sockPath, holder
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("forward control socket %s did not appear", sockPath)
	return "", nil
}

// TestTaskStart_ReRegistersPortsOnOriginalBindFace 是 R13/R21 的回归用例(计划要求先钉住这条):
// 已停止的任务 `start` 以同 id 重跑,其记录里的端口(含 -p 0.0.0.0:8080 的非回环绑定面)在本机
// 按原绑定面重新登记;运行中的任务 `start` 不重启进程,但转发规则照样补登记且不报冲突。
//
// 登记这一步必须由生产粘合 reRegisterTaskForwards 完成 —— 即 runTaskLifecycle 里交付 R13 的那一
// 行,而不是测试自己 sendForwardRegister。否则把那行删掉/挪到 `!restarted` 早退之后/传错 socket
// 路径,本文件仍会全绿,而「stop 之后再 start、或本机 node 重启后再 start,本地端口按原绑定面恢复」
// 的承诺会静默回退。nodeRunning=true 时 cfg 不参与,sockPath 是参数,故不触碰真实 node daemon。
func TestTaskStart_ReRegistersPortsOnOriginalBindFace(t *testing.T) {
	sockPath, holder := startTestForwardHolder(t)

	port := freeTCPPort(t)
	wantAddr := fmt.Sprintf("0.0.0.0:%d", port)
	recorded := []portfwd.Spec{{BindHost: "0.0.0.0", LocalPort: port, RemoteHost: "127.0.0.1", RemotePort: port}}

	// 1. 已停止的任务 + start:同 id 重跑,再由生产粘合按记录里的完整端口规格(含绑定主机)重登记。
	fb := &fakeTaskBackend{}
	task := backend.TaskInfo{ID: "t-1", Node: "eg-node", Status: "stopped", Cmd: "python -m http.server", Ports: recorded}
	ports, restarted, err := applyTaskLifecycle(verbStart, fb, task.Node, task)
	if err != nil || !restarted || len(fb.restarts) != 1 {
		t.Fatalf("start stopped task = (ports=%v restarted=%v restarts=%v err=%v), want one restart", ports, restarted, fb.restarts, err)
	}
	if err := reRegisterTaskForwards(nil, sockPath, task.ID, task.Node, ports, true); err != nil {
		t.Fatalf("reRegisterTaskForwards: %v", err)
	}
	// 生产路径的登记确实落地在 holder 上,且地址就是记录里的非回环绑定面。
	holder.mu.Lock()
	prod := holder.byTask[task.ID]
	holder.mu.Unlock()
	if len(prod) != 1 || prod[0].addr != wantAddr || prod[0].spec.BindHost != "0.0.0.0" || prod[0].listener == nil {
		t.Fatalf("holder after reRegisterTaskForwards = %+v, want one listening %s on 0.0.0.0", prod, wantAddr)
	}

	// 协议层再登记同一规格:返回的注册地址仍是原绑定面,且没有拒绝(KTD5 复用,不报冲突)。
	ptrs := make([]*PortForwardSpec, 0, len(ports))
	for i := range ports {
		ptrs = append(ptrs, &ports[i])
	}
	reg, rejected, err := sendForwardRegister(sockPath, task.ID, task.Node, ptrs)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(reg) != 1 || reg[0] != wantAddr {
		t.Fatalf("registered = %v, want [%s] (original non-loopback bind face)", reg, wantAddr)
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none", rejected)
	}

	// 2. 同一个任务已在运行时的 start:不重启进程(R2),但转发规则照样补登记;规则已存在时视为
	//    复用,不产生冲突告警(KTD5)—— 这正是「本机 node 重启后规则丢失,再 start 找回端口」的路径。
	fb2 := &fakeTaskBackend{}
	running := backend.TaskInfo{ID: task.ID, Node: task.Node, Status: taskStatusRunning, Cmd: "python -m http.server", Ports: recorded}
	ports2, restarted2, err := applyTaskLifecycle(verbStart, fb2, running.Node, running)
	if err != nil {
		t.Fatalf("start running task: %v", err)
	}
	if restarted2 || len(fb2.restarts) != 0 {
		t.Fatalf("R2 violated: start on running task restarted it (restarts=%v)", fb2.restarts)
	}
	if len(ports2) != 1 || ports2[0].BindHost != "0.0.0.0" {
		t.Fatalf("running task ports = %+v, want the recorded 0.0.0.0 spec", ports2)
	}
	if err := reRegisterTaskForwards(nil, sockPath, running.ID, running.Node, ports2, true); err != nil {
		t.Fatalf("reRegisterTaskForwards (running task): %v", err)
	}
	reg2 := make([]*PortForwardSpec, 0, len(ports2))
	for i := range ports2 {
		reg2 = append(reg2, &ports2[i])
	}
	// 同一任务、同一地址:复用,不报冲突。
	registered2, rejected2, err := sendForwardRegister(sockPath, running.ID, running.Node, reg2)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if len(registered2) != 1 || registered2[0] != wantAddr || len(rejected2) != 0 {
		t.Fatalf("reuse = (registered=%v rejected=%v), want [%s] and no rejection", registered2, rejected2, wantAddr)
	}

	// 3. 空规格:生产路径直接早退,连本地 node 都不去探活。这里故意传 nil cfg 且 nodeRunning=false,
	//    一旦早退被挪到 ensureLocalRelayNode 之后就会 panic(或去拨真实控制 socket)。
	if err := reRegisterTaskForwards(nil, sockPath, task.ID, task.Node, nil, false); err != nil {
		t.Fatalf("empty specs must be a no-op, got %v", err)
	}

	// 4. nodeRunning=false:先确保本地 node(转发持有者)在线。用一份解析不出 agent 角色的配置,
	//    并把 HOME 指到临时目录(daemon.baseDir 读 $HOME,故 PID 探测落在临时目录里)—— 既不拉起
	//    真实 node daemon,也不读写真实 ~/.relay,同时把「node 起不来」这一支钉成失败关闭:
	//    报错,而不是静默跳过登记。
	t.Setenv("HOME", t.TempDir())
	err = reRegisterTaskForwards(&config.Config{}, sockPath, "t-2", "eg-node", recorded, false)
	if err == nil {
		t.Fatal("node 未运行且不能自动拉起时必须报错,而不是静默跳过登记")
	}
	if !strings.Contains(err.Error(), "port-forward") || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("err = %v, want the port-forward wrap + agent-role guidance", err)
	}
	holder.mu.Lock()
	_, leaked := holder.byTask["t-2"]
	holder.mu.Unlock()
	if leaked {
		t.Fatal("node 未就绪时不得登记任何转发规则(失败关闭)")
	}

	// 规则确实按原绑定面在 holder 里活着。
	holder.mu.Lock()
	fw := holder.byTask["t-1"][0]
	bindOk := fw.spec.BindHost == "0.0.0.0"
	listenerOk := fw.listener != nil
	holder.mu.Unlock()
	if !bindOk || !listenerOk {
		t.Fatalf("holder entry: spec.BindHost=%q listenerOk=%v", fw.spec.BindHost, listenerOk)
	}

	if unResp := sendControl(t, sockPath, fwdControlMsg{Op: opUnregister, TaskID: "t-1"}); !unResp.OK {
		t.Fatalf("unregister: %s", unResp.Error)
	}
}

// TestForwardReRegister_TaskStartReusesOriginalBindFace 端到端覆盖 R13/R21/KTD5:`stop` 之后再
// `start`(或本机 node 重启后重登记)按任务记录里的完整端口规格(含绑定 0.0.0.0)重登记本机转发
// 规则,端口按原绑定面恢复可连;同一任务 + 同一地址重复登记视为复用,不报冲突;真正的跨任务占用
// 仍按冲突拒绝。
func TestForwardReRegister_TaskStartReusesOriginalBindFace(t *testing.T) {
	sockPath, holder := startTestForwardHolder(t)

	port := freeTCPPort(t)
	wantAddr := fmt.Sprintf("0.0.0.0:%d", port)
	// 任务记录里的完整规格:非回环绑定面(记录无损往返,KTD9/R21)。
	spec := PortForwardSpec{BindHost: "0.0.0.0", LocalPort: port, RemoteHost: "127.0.0.1", RemotePort: 9000}

	// 首次登记(stop 之后的 start):按记录的绑定面重登记。
	reg, rejected, err := sendForwardRegister(sockPath, "t1", "eg-node", []*PortForwardSpec{&spec})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(reg) != 1 || reg[0] != wantAddr {
		t.Fatalf("registered = %v, want [%s]", reg, wantAddr)
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v, want none", rejected)
	}

	// 同一任务 + 同一地址再次登记:幂等复用,不报冲突(KTD5/R2,「node 重启后规则丢失」的补登记)。
	reg, rejected, err = sendForwardRegister(sockPath, "t1", "eg-node", []*PortForwardSpec{&spec})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if len(reg) != 1 || reg[0] != wantAddr {
		t.Fatalf("reuse registered = %v, want [%s]", reg, wantAddr)
	}
	if len(rejected) != 0 {
		t.Fatalf("reuse rejected = %v, want none (no conflict warning)", rejected)
	}

	// 真正的跨任务占用仍按冲突拒绝。
	_, rejected, err = sendForwardRegister(sockPath, "t2", "eg-node", []*PortForwardSpec{&spec})
	if err != nil {
		t.Fatalf("cross-task register: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "already held by another forwarder") {
		t.Fatalf("cross-task rejected = %v, want one 'already held by another forwarder'", rejected)
	}

	// 持有者确实按记录的绑定面保留了转发(KTD5 复用不重建、不改变绑定面)。
	holder.mu.Lock()
	fw := holder.byTask["t1"][0]
	bindOk := fw.spec.BindHost == "0.0.0.0"
	listenerOk := fw.listener != nil
	holder.mu.Unlock()
	if !bindOk || !listenerOk {
		t.Fatalf("holder entry: spec.BindHost=%q listenerOk=%v", fw.spec.BindHost, listenerOk)
	}

	// 清理:卸载关闭监听,释放端口。
	unResp := sendControl(t, sockPath, fwdControlMsg{Op: opUnregister, TaskID: "t1"})
	if !unResp.OK {
		t.Fatalf("unregister: %s", unResp.Error)
	}
}
