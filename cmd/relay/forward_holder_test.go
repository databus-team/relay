package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
)

// 端到端: 测试 node-as-dockerd 端口转发持有者的 register -> 本地监听 -> 隧道透传 -> unregister 关闭流程。
// 模拟执行方上的 Echo 服务,中转服务器开启隧道,持有者通过 unix socket 接受 register 控制,
// 建立本地端口转发并发送真实流量验证全双工透传,再通过 unregister 回收监听。
func TestEndToEnd_ForwardHolder_RegisterEchoUnregister(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 远端模拟 TCP Echo 服务
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	remotePort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// 2. 中转服务器(开启隧道)
	watchDir := t.TempDir()
	srv, err := server.New(server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{"tok-exe", "tok-req"},
		},
		TunnelEnabled: true,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	// 3. 执行方注册
	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-agent",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 请求方(持有者端口转转后端)
	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "cli-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	// 5. 取一个空闲本地端口
	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	localPort := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	// 6. 构造持有者并监听 unix 控制 socket(用一个短路径:macOS unix socket 有 ~104 字节路径上限)
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holder := newForwardHolder(ctx, rb, sockPath)
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- holder.Run() // 由 ctx 取消结束
	}()

	// 等待 socket 就绪
	waitSock := func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-holderErr:
				t.Fatalf("holder.Run returned: %v", err)
			default:
			}
			if _, err := os.Stat(sockPath); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("forward control socket %s did not appear", sockPath)
	}
	waitSock()

	// 7. register: 经 unix socket 提交 task 端口转发
	resp := sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-abc",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK {
		t.Fatalf("register failed: %s", resp.Error)
	}
	if len(resp.Registered) != 1 || resp.Registered[0] != localAddr {
		t.Fatalf("registered = %v, want [%s]", resp.Registered, localAddr)
	}

	// 8. 向本地转发端口发流,验证透传远端 Echo
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local forward %s: %v", localAddr, err)
	}
	testMsg := "hello-forward-holder\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to local port: %v", err)
	}
	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from local port: %v", err)
	}
	conn.Close()
	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}

	// 9. register 同一端口冲突:应 rejected 而不影响已有转发
	resp = sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-other",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK {
		t.Fatalf("conflicting register failed: %s", resp.Error)
	}
	if len(resp.Rejected) != 1 {
		t.Fatalf("conflicting register rejected = %v, want 1 rejection", resp.Rejected)
	}

	// 10. 验证端口仍可连(未被错误覆盖)
	conn2, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("port should still listen after conflict: %v", err)
	}
	conn2.Close()

	// 11. unregister: 关闭该 task 的全部转发
	unResp := sendControl(t, sockPath, fwdControlMsg{Op: opUnregister, TaskID: "task-abc"})
	if !unResp.OK {
		t.Fatalf("unregister failed: %s", unResp.Error)
	}

	// 12. 验证本地监听已关闭
	time.Sleep(50 * time.Millisecond)
	_, dialErr := net.DialTimeout("tcp", localAddr, 200*time.Millisecond)
	if dialErr == nil {
		t.Fatalf("expected dial to fail after unregister, but succeeded")
	}
}

// sendControl 通过 unix socket 发送一条控制消息并读取应答。
func sendControl(t *testing.T, sockPath string, msg fwdControlMsg) fwdControlResp {
	t.Helper()
	resp, err := sendForwardControl(sockPath, msg)
	if err != nil {
		t.Fatalf("send control %s: %v", msg.Op, err)
	}
	return resp
}

// startForwardHolderControl 启动一个只测控制面的 holder(rb=nil:open/close/restart/list 不触碰
// 数据面,仅真实连接透传才需要 backend)。返回 sockPath 与停止函数。
func startForwardHolderControl(t *testing.T) (sockPath string, stop func()) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	sockPath = filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	holder := newForwardHolder(ctx, nil, sockPath)
	holder.statePath = filepath.Join(sockDir, "forwards.json")
	go func() { _ = holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return sockPath, func() { cancel(); os.RemoveAll(sockDir) }
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	os.RemoveAll(sockDir)
	t.Fatalf("forward control socket %s did not appear", sockPath)
	return "", nil
}

// freeLocalPort 找一个空闲端口(绑定后立即释放,供随后转发占用)。
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestForwardHolder_OpenCloseList 覆盖独立转发(a.k.a. port-forward-only)的控制面生命周期:
// open 幂等、list 展示 running、冲突检测、close 释放端口并移除意图。
func TestForwardHolder_OpenCloseList(t *testing.T) {
	sockPath, stop := startForwardHolderControl(t)
	defer stop()
	localPort := freeLocalPort(t)
	spec := func() *PortForwardSpec {
		return &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: 9090}
	}

	// 1. open 建独立转发。
	resp := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: localPort, Executor: "eg-x", Spec: spec(), Restart: tunRestartAlways})
	if !resp.OK {
		t.Fatalf("open failed: %s", resp.Error)
	}

	// 2. list:一条 running,身份=端口,executor 正确。
	lst := sendControl(t, sockPath, fwdControlMsg{Op: opList})
	if len(lst.Forwards) != 1 {
		t.Fatalf("list after open = %d forwards, want 1", len(lst.Forwards))
	}
	f := lst.Forwards[0]
	if f.Port != localPort || f.Executor != "eg-x" || f.State != "running" || f.Restart != tunRestartAlways {
		t.Fatalf("listed forward = %+v, want port=%d executor=eg-x running always", f, localPort)
	}

	// 3. 幂等:同端口再 open 不重复、只更新策略。
	resp2 := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: localPort, Executor: "eg-x", Spec: spec(), Restart: tunRestartNo})
	if !resp2.OK {
		t.Fatalf("reopen failed: %s", resp2.Error)
	}
	lst2 := sendControl(t, sockPath, fwdControlMsg{Op: opList})
	if len(lst2.Forwards) != 1 || lst2.Forwards[0].Restart != tunRestartNo {
		t.Fatalf("reopen produced %d forwards with restart=%q, want 1 with no", len(lst2.Forwards), lst2.Forwards[0].Restart)
	}

	// 4. close 关闭并移除。状态测试不拨端口(拨端口会触发数据面 pump,需要真实 backend;
	// 透传行为已由任务转发 E2E 覆盖,独立转发共用同一 handlePortForwardConn)。
	cl := sendControl(t, sockPath, fwdControlMsg{Op: opClose, Port: localPort})
	if !cl.OK {
		t.Fatalf("close failed: %s", cl.Error)
	}
	lst3 := sendControl(t, sockPath, fwdControlMsg{Op: opList})
	if len(lst3.Forwards) != 0 {
		t.Fatalf("list after close = %d forwards, want 0", len(lst3.Forwards))
	}
	// 未监听才允许无缝重建:同端口再 open 应成功。
	reopen := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: localPort, Executor: "eg-x", Spec: spec(), Restart: tunRestartAlways})
	if !reopen.OK {
		t.Fatalf("reopen after close failed: %s", reopen.Error)
	}
}

// TestForwardHolder_OpenConflictWithTask 验证独立转发与任务绑定转发的 byAddr 冲突检测:
// 同端口已被任务转发占用时 open 必须拒绝,不得误覆盖。
func TestForwardHolder_OpenConflictWithTask(t *testing.T) {
	sockPath, stop := startForwardHolderControl(t)
	defer stop()
	localPort := freeLocalPort(t)

	// 任务绑定转发先占住端口。
	r := sendControl(t, sockPath, fwdControlMsg{
		Op: opRegister, TaskID: "task-held", Executor: "eg-x",
		Specs: []PortForwardSpec{{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: 9090}},
	})
	if !r.OK || len(r.Registered) != 1 {
		t.Fatalf("task register failed: %+v", r)
	}

	// 独立转发 open 同端口 → 拒绝。
	o := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: localPort, Executor: "eg-x", Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: 9090}})
	if o.OK {
		t.Fatalf("open over task-held port should be rejected, got OK")
	}
	if !strings.Contains(o.Error, "already held by another forwarder") {
		t.Fatalf("rejection message = %q, want 'already held by another forwarder'", o.Error)
	}
	// 任务转发仍完好。
	if lst := sendControl(t, sockPath, fwdControlMsg{Op: opList}); len(lst.Forwards) != 0 {
		t.Fatalf("task-held forward leaked into standalone list: %+v", lst.Forwards)
	}
}

// TestForwardHolder_RestorePolicies 覆盖 restore 按 restart 策略恢复独立转发:
// always 重新绑定监听;no 只留 stopped 意图(list 可见、端口不监听)。
func TestForwardHolder_RestorePolicies(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	p1 := freeLocalPort(t)
	p2 := freeLocalPort(t)

	file := forwardStateFile{Forwards: []forwardState{
		{Port: p1, Executor: "eg-x", Spec: PortForwardSpec{BindHost: "127.0.0.1", LocalPort: p1, RemoteHost: "127.0.0.1", RemotePort: 9090}, Restart: tunRestartAlways, State: "running", Started: time.Now()},
		{Port: p2, Executor: "eg-y", Spec: PortForwardSpec{BindHost: "127.0.0.1", LocalPort: p2, RemoteHost: "127.0.0.1", RemotePort: 9091}, Restart: tunRestartNo, State: "running", Started: time.Now()},
	}}
	if err := persistForwards(filepath.Join(sockDir, "forwards.json"), file); err != nil {
		t.Fatalf("persist fixture: %v", err)
	}

	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := newForwardHolder(ctx, nil, sockPath)
	holder.statePath = filepath.Join(sockDir, "forwards.json")
	go func() { _ = holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	lst := sendControl(t, sockPath, fwdControlMsg{Op: opList})
	if len(lst.Forwards) != 2 {
		t.Fatalf("restored forwards = %d, want 2", len(lst.Forwards))
	}
	byPort := map[int]forwardState{}
	for _, f := range lst.Forwards {
		byPort[f.Port] = f
	}
	if byPort[p1].State != "running" {
		t.Fatalf("restart=always (%d) state = %q, want running", p1, byPort[p1].State)
	}
	if byPort[p2].State != "stopped" {
		t.Fatalf("restart=no (%d) state = %q, want stopped", p2, byPort[p2].State)
	}
	// running 状态即隐含监听已成功绑定(bindForwardLocked 失败会降为 stopped);不拨端口以免触发数据面。
}

// TestForwardHolder_RestoreStateFileUpgrade 验证 restore 之后任意一次 persist 会把旧数组格式升级为
// 对象格式,且任务绑定转发保留(tasks 列表)。
func TestForwardHolder_RestoreStateFileUpgrade(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	statePath := filepath.Join(sockDir, "forwards.json")

	// 写旧格式数组(含一个任务绑定转发)。
	taskPort := freeLocalPort(t)
	old := []fwdIntent{{TaskID: "t-upgrade", Executor: "eg-x", Specs: []PortForwardSpec{{BindHost: "127.0.0.1", LocalPort: taskPort, RemoteHost: "127.0.0.1", RemotePort: 1}}}}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := newForwardHolder(ctx, nil, sockPath)
	holder.statePath = statePath
	go func() { _ = holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 任意一次触发 persist 的操作(这里是同任务幂等 register)会把文件写成对象格式。
	sendControl(t, sockPath, fwdControlMsg{Op: opRegister, TaskID: "t-upgrade", Executor: "eg-x", Specs: []PortForwardSpec{{BindHost: "127.0.0.1", LocalPort: taskPort, RemoteHost: "127.0.0.1", RemotePort: 1}}})

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(raw), "\"tasks\"") || !strings.Contains(string(raw), "\"forwards\"") {
		t.Fatalf("state file not upgraded to object format: %s", string(raw))
	}
}

// TestForwardHolder_PersistSeparation 钉住 persist 的回归点:任务绑定转发与独立转发必须分列表
// 持久化(tasks/forwards),register + open 混用后 reload 各恰一条,独立转发绝不进 tasks
// (此前若挂进 byTask 会被 restore 的空 TaskID 分支跳过而静默丢失)。
func TestForwardHolder_PersistSeparation(t *testing.T) {
	sockPath, stop := startForwardHolderControl(t)
	defer stop()
	taskPort := freeLocalPort(t)
	fwdPort := freeLocalPort(t)
	statePath := filepath.Join(filepath.Dir(sockPath), "forwards.json")

	// 任务绑定转发 + 独立转发并存。
	r := sendControl(t, sockPath, fwdControlMsg{
		Op: opRegister, TaskID: "task-x", Executor: "eg-x",
		Specs: []PortForwardSpec{{BindHost: "127.0.0.1", LocalPort: taskPort, RemoteHost: "127.0.0.1", RemotePort: 1}},
	})
	if !r.OK || len(r.Registered) != 1 {
		t.Fatalf("task register failed: %+v", r)
	}
	o := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: fwdPort, Executor: "eg-y", Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: fwdPort, RemoteHost: "127.0.0.1", RemotePort: 2}, Restart: tunRestartAlways})
	if !o.OK {
		t.Fatalf("open failed: %s", o.Error)
	}

	file, err := loadForwards(statePath)
	if err != nil {
		t.Fatalf("loadForwards: %v", err)
	}
	if len(file.Tasks) != 1 || file.Tasks[0].TaskID != "task-x" {
		t.Fatalf("tasks = %+v, want exactly one task-x", file.Tasks)
	}
	if len(file.Forwards) != 1 || file.Forwards[0].Port != fwdPort {
		t.Fatalf("forwards = %+v, want exactly one port=%d", file.Forwards, fwdPort)
	}
}

// TestLoadForwards_LegacyArray 验证旧格式(forwards.json 顶层是 fwdIntent 数组)能被新代码读取,
// 全部按任务绑定转发归入 tasks。
func TestLoadForwards_LegacyArray(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "forwards.json")
	old := []fwdIntent{
		{TaskID: "t-legacy", Executor: "eg-x", Specs: []PortForwardSpec{{BindHost: "127.0.0.1", LocalPort: 7001, RemoteHost: "127.0.0.1", RemotePort: 7001}}},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	file, err := loadForwards(statePath)
	if err != nil {
		t.Fatalf("loadForwards legacy: %v", err)
	}
	if len(file.Tasks) != 1 || file.Tasks[0].TaskID != "t-legacy" || file.Tasks[0].Specs[0].LocalPort != 7001 {
		t.Fatalf("legacy tasks = %+v, want one t-legacy/7001", file.Tasks)
	}
	if len(file.Forwards) != 0 {
		t.Fatalf("legacy forwards = %+v, want none", file.Forwards)
	}

	// 迁移写入为对象格式后,再读仍是同一组任务。
	if err := persistForwards(statePath, file); err != nil {
		t.Fatalf("persist migrated: %v", err)
	}
	reloaded, err := loadForwards(statePath)
	if err != nil {
		t.Fatalf("reload migrated: %v", err)
	}
	if len(reloaded.Tasks) != 1 || reloaded.Tasks[0].TaskID != "t-legacy" {
		t.Fatalf("reloaded tasks = %+v, want legacy preserved", reloaded.Tasks)
	}
}

// TestForwardHolder_Restart 覆盖独立转发的 restart 控制:关闭后按原参数重开,list 仍一条 running。
func TestForwardHolder_Restart(t *testing.T) {
	sockPath, stop := startForwardHolderControl(t)
	defer stop()
	localPort := freeLocalPort(t)

	o := sendControl(t, sockPath, fwdControlMsg{Op: opOpen, Port: localPort, Executor: "eg-x", Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: 9090}, Restart: tunRestartAlways})
	if !o.OK {
		t.Fatalf("open failed: %s", o.Error)
	}
	rs := sendControl(t, sockPath, fwdControlMsg{Op: opRestart, Port: localPort})
	if !rs.OK {
		t.Fatalf("restart failed: %s", rs.Error)
	}
	lst := sendControl(t, sockPath, fwdControlMsg{Op: opList})
	if len(lst.Forwards) != 1 || lst.Forwards[0].State != "running" || lst.Forwards[0].Executor != "eg-x" {
		t.Fatalf("after restart = %+v, want one running eg-x", lst.Forwards)
	}
	// running 状态即隐含重开成功(bindForwardLocked 失败会返回错误);不拨端口以免触发数据面。
}

func TestCaptureDetachTaskID(t *testing.T) {
	cases := []struct {
		name   string
		data   string
		wantID string
		wantOK bool
	}{
		{"marker 精确解析", protocol.ForwardTaskIDMarker + "t-abc", "t-abc", true},
		{"marker 带空白", protocol.ForwardTaskIDMarker + "t-abc\n", "t-abc", true},
		{"marker 但空 ID 不捕获", protocol.ForwardTaskIDMarker + "   \n", "", false},
		{"非 marker 行不捕获", "t-old", "", false},
		{"空行不捕获", "\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := captureDetachTaskID(tc.data)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("captureDetachTaskID(%q) = (%q,%v), want (%q,%v)", tc.data, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestEndToEnd_ForwardHolder_RestoreOnRestart 验证 node restart 后 forwardHolder.restore()
// 自动恢复持久化的端口监听，并恢复透传。
func TestEndToEnd_ForwardHolder_RestoreOnRestart(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	remotePort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	watchDir := t.TempDir()
	srv, err := server.New(server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{"tok-exe", "tok-req"},
		},
		TunnelEnabled: true,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-agent",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "cli-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	localPort := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "forward.sock")
	statePath := filepath.Join(sockDir, "forwards.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holder := newForwardHolder(ctx, rb, sockPath)
	holder.statePath = statePath
	go func() { _ = holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp := sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-restart-test",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK || len(resp.Registered) != 1 {
		t.Fatalf("initial register failed: %+v", resp)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if !strings.Contains(string(data), "task-restart-test") {
		t.Fatalf("state file does not contain task id: %s", string(data))
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	holder.mu.Lock()
	for _, fw := range holder.byAddr {
		_ = fw.listener.Close()
	}
	holder.mu.Unlock()

	sockPath2 := filepath.Join(sockDir, "forward2.sock")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	holder2 := newForwardHolder(ctx2, rb, sockPath2)
	holder2.statePath = statePath
	go func() { _ = holder2.Run() }()

	deadline2 := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline2) {
		if _, err := os.Stat(sockPath2); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial restored forward %s failed: %v", localAddr, err)
	}
	testMsg := "hello-restored-forward\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to restored port: %v", err)
	}
	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from restored port: %v", err)
	}
	conn.Close()
	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}
}

// TestEndToEnd_ForwardHolder_OpenEchoClose 走独立转发(port-forward-only)的完整数据面:
// open 建监听 → 拨本地端口走中转隧道到远端 Echo → 全双工透传 → close 释放。
// 与 RegisterEchoUnregister 的区别只是控制消息换成 open/close、且不挂任务。
func TestEndToEnd_ForwardHolder_OpenEchoClose(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	remotePort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	watchDir := t.TempDir()
	srv, err := server.New(server.Config{Addr: ":0", WatchDirs: []server.WatchDirConfig{{ID: "test", Dir: watchDir}}, Auth: server.AuthConfig{Type: "token", Tokens: []string{"tok-exe", "tok-req"}}, TunnelEnabled: true})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "node-agent", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "cli-client", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	localPort := freeLocalPort(t)
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := newForwardHolder(ctx, rb, sockPath)
	go func() { _ = holder.Run() }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// open 独立转发(不挂任务)。
	resp := sendControl(t, sockPath, fwdControlMsg{
		Op: opOpen, Port: localPort, Executor: "node-agent",
		Spec:     &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		Restart:  tunRestartAlways,
	})
	if !resp.OK {
		t.Fatalf("open failed: %s", resp.Error)
	}

	// 拨本地转发端口,验证远端 Echo 透传。
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local forward %s: %v", localAddr, err)
	}
	testMsg := "hello-port-forward-only\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to local port: %v", err)
	}
	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from local port: %v", err)
	}
	conn.Close()
	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}

	// close 释放。
	cl := sendControl(t, sockPath, fwdControlMsg{Op: opClose, Port: localPort})
	if !cl.OK {
		t.Fatalf("close failed: %s", cl.Error)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", localAddr, 200*time.Millisecond); err == nil {
		t.Fatalf("expected dial to fail after close, but succeeded")
	}
}
