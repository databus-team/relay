package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer 是一个并发安全的日志缓冲:持有者 goroutine 写、测试 goroutine 读,
// 经内部互斥锁串行,避免 -race 报竞态(替代交换进程级 os.Stderr 的做法)。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeTCPPort 取一个当前空闲的本地 TCP 端口(探测后立即释放,竞态可忽略)。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// startTestHolder 在临时目录启动一个隧道持有者(nil 后端,隧道意图只需监听本地端口即验证),
// 等待 unix 控制 socket 就绪后返回 holder、cancel 与 sockPath。statePath 为空时默认指向该临时目录
// (须在启动前设定,避免与 Run 内 restore 的读取竞态)。logw 非空时注入为日志目标以便捕获。
func startTestHolder(t *testing.T, statePath string, logw io.Writer) (*tunnelHolder, context.CancelFunc, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "relay-tun-*")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	sockPath := filepath.Join(sockDir, "tunnel.sock")
	ctx, cancel := context.WithCancel(context.Background())
	holder := newTunnelHolder(ctx, nil, sockPath)
	if statePath == "" {
		statePath = filepath.Join(sockDir, "tunnels.json")
	}
	holder.statePath = statePath
	if logw != nil {
		holder.logw = logw
	}

	holderErr := make(chan error, 1)
	go func() {
		holderErr <- holder.Run() // 由 ctx 取消结束
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-holderErr:
			t.Fatalf("holder.Run returned: %v", err)
		default:
		}
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(cancel)
	return holder, cancel, sockPath
}

// sendTunControl 经 unix socket 发送一条隧道控制消息并读取应答。
func sendTunControl(t *testing.T, sockPath string, msg tunControlMsg) tunControlResp {
	t.Helper()
	resp, err := sendTunnelControl(sockPath, msg)
	if err != nil {
		t.Fatalf("send tunnel control %s: %v", msg.Op, err)
	}
	return resp
}

// openTunnel 便捷封装:发送 open 并断言成功。
func openTunnel(t *testing.T, sockPath, egress, listen, restart string) {
	t.Helper()
	resp := sendTunControl(t, sockPath, tunControlMsg{Op: tunOpOpen, Egress: egress, Listen: listen, Restart: restart})
	if !resp.OK {
		t.Fatalf("open tunnel %s failed: %s", egress, resp.Error)
	}
}

// TestTunnelHolder_StateFileRoundTrip 验证 tunnels.json 的持久化/读取往返保持 egress/listen/restart 及运行态。
func TestTunnelHolder_StateFileRoundTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) // 无单调时钟,往返可比
	states := []tunnelState{
		{Egress: "eg1", Listen: "127.0.0.1:1080", Restart: tunRestartAlways, State: "running", Started: ts},
		{Egress: "eg2", Listen: "0.0.0.0:9090", Restart: tunRestartNo, State: "stopped"},
	}

	if err := persistTunnels(statePath, states); err != nil {
		t.Fatalf("persistTunnels: %v", err)
	}
	loaded, err := loadTunnels(statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	if len(loaded) != len(states) {
		t.Fatalf("loaded %d states, want %d", len(loaded), len(states))
	}
	for i := range states {
		if loaded[i].Egress != states[i].Egress || loaded[i].Listen != states[i].Listen ||
			loaded[i].Restart != states[i].Restart || loaded[i].State != states[i].State ||
			!loaded[i].Started.Equal(states[i].Started) {
			t.Fatalf("round-trip mismatch at %d: got %+v, want %+v", i, loaded[i], states[i])
		}
	}
}

// TestTunnelHolder_OpenIdempotentClose 验证 open 同一 egress 幂等(二次 open 安全),close 移除条目与持久化意图。
func TestTunnelHolder_OpenIdempotentClose(t *testing.T) {
	holder, cancel, sock := startTestHolder(t, "", nil)
	defer cancel()

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)

	openTunnel(t, sock, "eg", listen, tunRestartNo)

	// 二次 open 同一 egress:应为幂等的 close-and-reopen,返回 OK。
	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpOpen, Egress: "eg", Listen: listen, Restart: tunRestartNo})
	if !resp.OK {
		t.Fatalf("second open failed: %s", resp.Error)
	}

	// list 应恰好一条 running(close-and-reopen 不会重复累加)。
	listResp := sendTunControl(t, sock, tunControlMsg{Op: tunOpList})
	if !listResp.OK {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	if len(listResp.Tunnels) != 1 || listResp.Tunnels[0].Egress != "eg" || listResp.Tunnels[0].State != "running" {
		t.Fatalf("unexpected list result: %+v", listResp.Tunnels)
	}

	// 持久化意图存在且指向 eg。
	states, err := loadTunnels(holder.statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	if len(states) != 1 || states[0].Egress != "eg" {
		t.Fatalf("persisted state = %+v, want one egress=eg", states)
	}

	// close 移除条目并清空持久化意图。
	resp = sendTunControl(t, sock, tunControlMsg{Op: tunOpClose, Egress: "eg"})
	if !resp.OK {
		t.Fatalf("close failed: %s", resp.Error)
	}
	states, err = loadTunnels(holder.statePath)
	if err != nil {
		t.Fatalf("loadTunnels after close: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("persisted state after close = %+v, want empty", states)
	}

	// 端口已释放,无法再连。
	time.Sleep(50 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", listen, 200*time.Millisecond); err == nil {
		t.Fatalf("expected dial to fail after close, but succeeded")
	}
}

// TestTunnelHolder_CloseClosesInFlight 验证 close 会关闭已 accept 的在途连接,随后端口不再可连。
func TestTunnelHolder_CloseClosesInFlight(t *testing.T) {
	holder, cancel, sock := startTestHolder(t, "", nil)
	defer cancel()

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	openTunnel(t, sock, "eg-inflight", listen, tunRestartNo)

	// 拨号一个 SOCKS 客户端:只建连不发握手字节,使 handleTunnelConn 阻塞在 SOCKS5 握手阶段。
	client, err := net.DialTimeout("tcp", listen, 2*time.Second)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer client.Close()

	// 等待该连接被 serveEntry 登记到 entry.conns(close 前必须已登记,否则不会被关闭)。
	registered := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		holder.mu.Lock()
		e, ok := holder.byEgress["eg-inflight"]
		n := 0
		if ok {
			n = len(e.conns)
		}
		holder.mu.Unlock()
		if n >= 1 {
			registered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !registered {
		t.Fatalf("in-flight conn was not registered in entry.conns before close")
	}

	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpClose, Egress: "eg-inflight"})
	if !resp.OK {
		t.Fatalf("close failed: %s", resp.Error)
	}

	// 在途连接被持有者关闭:对端读到 EOF/错误。
	buf := make([]byte, 1)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Read(buf); err == nil {
		t.Fatalf("expected in-flight conn to be closed, but read succeeded")
	}

	// 端口不再接受新连接。
	time.Sleep(50 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", listen, 200*time.Millisecond); err == nil {
		t.Fatalf("expected dial to fail after close, but succeeded")
	}
}

// TestTunnelHolder_LogPrefix 验证持有者的连接级/状态级日志使用 `[tunnel <egress>]` 前缀。
// 通过把 statePath 指向一个父路径为普通文件的不可写位置,强制 opOpen 的持久化失败并打印带前缀日志,
// 日志经注入的并发安全缓冲捕获(不触碰进程级 os.Stderr)。
func TestTunnelHolder_LogPrefix(t *testing.T) {
	// 父路径是普通文件 -> MkdirAll 失败 -> persist 报错,触发 `[tunnel <egress>]` 前缀日志。
	notadir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(notadir, []byte("x"), 0o644); err != nil {
		t.Fatalf("create notadir: %v", err)
	}
	logs := &syncBuffer{}
	_, cancel, sock := startTestHolder(t, filepath.Join(notadir, "tunnels.json"), logs)
	defer cancel()

	openTunnel(t, sock, "eg-log", "127.0.0.1:0", tunRestartNo)
	if !strings.Contains(logs.String(), "[tunnel eg-log]") {
		t.Fatalf("holder log does not use [tunnel <egress>] prefix; got:\n%s", logs.String())
	}
}

// TestTunnelHolder_NonLoopbackWarning 验证 open 与 restore 两个路径在绑定非 loopback 地址时都发出告警。
// 选用 TEST-NET-1 保留地址(192.0.2.1,非本机接口):告警在 Listen 之前发出,随后 bind 失败属预期,
// 从而既断言告警、又避免在真实非 loopback 接口上开监听(规避 macOS 防火墙弹窗/CI 挂起)。
func TestTunnelHolder_NonLoopbackWarning(t *testing.T) {
	const nonLoopIP = "192.0.2.1:0"

	// open 路径:在 listen 端不绑定真实非 loopback 接口,使用注入日志缓冲捕获告警。
	openLogs := &syncBuffer{}
	_, cancel, sock := startTestHolder(t, "", openLogs)
	defer cancel()
	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpOpen, Egress: "eg-warn", Listen: nonLoopIP, Restart: tunRestartAlways})
	_ = resp // bind 失败属预期,告警才是断言目标
	if !strings.Contains(openLogs.String(), `WARNING: listening on "192.0.2.1:0" (non-loopback). This tunnel has no auth; any host that can reach this port can use it to reach the executor's network.`) {
		t.Fatalf("open non-loopback warning not emitted; got:\n%s", openLogs.String())
	}

	// restore 路径同样在 bind 前发出告警。
	restoreLogs := &syncBuffer{}
	restoreHolder := newTunnelHolder(context.Background(), nil, filepath.Join(t.TempDir(), "tunnel.sock"))
	restoreHolder.logw = restoreLogs
	restoreHolder.statePath = filepath.Join(t.TempDir(), "restore.json")
	if err := persistTunnels(restoreHolder.statePath, []tunnelState{{Egress: "eg-warn2", Listen: nonLoopIP, Restart: tunRestartAlways}}); err != nil {
		t.Fatalf("persist state: %v", err)
	}
	restoreHolder.restore()
	if !strings.Contains(restoreLogs.String(), `WARNING: listening on "192.0.2.1:0" (non-loopback).`) {
		t.Fatalf("restore non-loopback warning not emitted; got:\n%s", restoreLogs.String())
	}
}

// nonBindableListen 是一个本机绑不上的地址(TEST-NET-1 保留段,非本机接口地址),
// 用于确定性地制造 net.Listen 失败(EADDRNOTAVAIL),避免依赖端口占用这类易漂移的条件。
const nonBindableListen = "192.0.2.1:0"

// waitDialable 轮询直到 listen 可建立 TCP 连接(监听已就绪),超时即失败。
func waitDialable(t *testing.T, listen string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", listen, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("address %s never became dialable", listen)
}

// TestTunnelHolder_Restart 验证根级 `relay restart <egress>` 的下发动作(tunOpRestart)在原地址上关闭后重开:
// 应答 OK、list 中仍是同一条 running 条目(不重复累加)、地址重新可连、持久化意图保持。
func TestTunnelHolder_Restart(t *testing.T) {
	holder, cancel, sock := startTestHolder(t, "", nil)
	defer cancel()

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	openTunnel(t, sock, "eg-restart", listen, tunRestartAlways)
	waitDialable(t, listen)

	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpRestart, Egress: "eg-restart"})
	if !resp.OK {
		t.Fatalf("restart failed: %s", resp.Error)
	}

	listResp := sendTunControl(t, sock, tunControlMsg{Op: tunOpList})
	if !listResp.OK {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	if len(listResp.Tunnels) != 1 {
		t.Fatalf("list after restart = %+v, want exactly one entry", listResp.Tunnels)
	}
	got := listResp.Tunnels[0]
	if got.Egress != "eg-restart" || got.Listen != listen || got.State != "running" || got.Restart != tunRestartAlways {
		t.Fatalf("list after restart = %+v, want egress=eg-restart listen=%s state=running restart=always", got, listen)
	}

	// 原地址重新可连(重启后新监听接管)。
	waitDialable(t, listen)

	states, err := loadTunnels(holder.statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	if len(states) != 1 || states[0].Egress != "eg-restart" || states[0].State != "running" {
		t.Fatalf("persisted state after restart = %+v, want one running eg-restart", states)
	}
}

// TestTunnelHolder_RestartBindFailureKeepsStoppedEntry 验证 restart 重绑失败时的语义:
// 应答 OK=false、条目仍以 stopped 留在内存(list 可见,可再次 restart),且意图文件保持原样不被改写。
func TestTunnelHolder_RestartBindFailureKeepsStoppedEntry(t *testing.T) {
	holder, cancel, sock := startTestHolder(t, "", nil)
	defer cancel()

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	openTunnel(t, sock, "eg-rfail", listen, tunRestartAlways)
	waitDialable(t, listen)

	before, err := os.ReadFile(holder.statePath)
	if err != nil {
		t.Fatalf("read state before restart: %v", err)
	}

	// 白盒:把条目的 listen 改成一个本机绑不上的地址,确定性地制造重绑失败。
	holder.mu.Lock()
	holder.byEgress["eg-rfail"].listen = nonBindableListen
	holder.mu.Unlock()

	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpRestart, Egress: "eg-rfail"})
	if resp.OK {
		t.Fatalf("restart with unbindable listen unexpectedly succeeded: %+v", resp)
	}

	// 条目仍在,但降为 stopped(可被再次 restart 恢复)。
	listResp := sendTunControl(t, sock, tunControlMsg{Op: tunOpList})
	if len(listResp.Tunnels) != 1 || listResp.Tunnels[0].Egress != "eg-rfail" || listResp.Tunnels[0].State != "stopped" {
		t.Fatalf("list after failed restart = %+v, want one stopped eg-rfail", listResp.Tunnels)
	}

	// 失败路径不落盘:tunnels.json 与 restart 前逐字节一致。
	after, err := os.ReadFile(holder.statePath)
	if err != nil {
		t.Fatalf("read state after failed restart: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("state file changed on failed restart:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestTunnelHolder_OpenReplaceBindFailureKeepsRunning 验证 replace 的原子性:重复 open 同一 egress
// 时若新监听绑不上,原有 running 隧道必须原样存活(不能被一条失败的命令杀掉),且不进 stopped。
func TestTunnelHolder_OpenReplaceBindFailureKeepsRunning(t *testing.T) {
	_, cancel, sock := startTestHolder(t, "", nil)
	defer cancel()

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	openTunnel(t, sock, "eg-keep", listen, tunRestartAlways)
	waitDialable(t, listen)

	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpOpen, Egress: "eg-keep", Listen: nonBindableListen, Restart: tunRestartAlways})
	if resp.OK {
		t.Fatalf("open with unbindable listen unexpectedly succeeded: %+v", resp)
	}

	listResp := sendTunControl(t, sock, tunControlMsg{Op: tunOpList})
	if len(listResp.Tunnels) != 1 || listResp.Tunnels[0].Egress != "eg-keep" ||
		listResp.Tunnels[0].Listen != listen || listResp.Tunnels[0].State != "running" {
		t.Fatalf("list after failed replace = %+v, want the original running eg-keep on %s", listResp.Tunnels, listen)
	}

	// 旧监听未被拆除,端口仍可连。
	waitDialable(t, listen)
}

// TestTunnelHolder_PersistFailureSurfacesWarning 验证持久化失败不再被吞成静默成功:应答仍 OK
// (监听器确实已开),但必须携带 Warning 让 CLI 能打到 stderr。复用 LogPrefix 的不可写路径构造。
func TestTunnelHolder_PersistFailureSurfacesWarning(t *testing.T) {
	notadir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(notadir, []byte("x"), 0o644); err != nil {
		t.Fatalf("create notadir: %v", err)
	}
	logs := &syncBuffer{}
	_, cancel, sock := startTestHolder(t, filepath.Join(notadir, "tunnels.json"), logs)
	defer cancel()

	resp := sendTunControl(t, sock, tunControlMsg{Op: tunOpOpen, Egress: "eg-warn-persist", Listen: "127.0.0.1:0", Restart: tunRestartAlways})
	if !resp.OK {
		t.Fatalf("open failed: %s", resp.Error)
	}
	if resp.Warning == "" {
		t.Fatalf("persist failure was swallowed: resp.Warning is empty (%+v); logs:\n%s", resp, logs.String())
	}

	// close 的持久化失败同样必须带后果说明(意图仍在盘上 -> 下次 node 启动会复活它)。
	closeResp := sendTunControl(t, sock, tunControlMsg{Op: tunOpClose, Egress: "eg-warn-persist"})
	if !closeResp.OK {
		t.Fatalf("close failed: %s", closeResp.Error)
	}
	if !strings.Contains(closeResp.Warning, "reappear") {
		t.Fatalf("close persist warning does not state the consequence: %q", closeResp.Warning)
	}
}

// errListener 是一个只用于单测的监听器替身:Accept 立即返回固定错误,用于模拟非主动关闭的
// Accept 失败(EMFILE/ENOBUFS/ECONNABORTED 这类真实故障)。
type errListener struct {
	err    error
	addr   net.Addr
	closed bool
}

func (l *errListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *errListener) Close() error              { l.closed = true; return nil }
func (l *errListener) Addr() net.Addr {
	if l.addr == nil {
		return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
	}
	return l.addr
}

// TestTunnelHolder_AcceptErrorMarksStopped 验证非主动关闭的 Accept 失败不会让条目继续谎报 running:
// serveEntry 记一条日志并把 listener 置空,toState 随之降为 stopped(可被 restart 重新拉起)。
func TestTunnelHolder_AcceptErrorMarksStopped(t *testing.T) {
	logs := &syncBuffer{}
	holder := newTunnelHolder(context.Background(), nil, filepath.Join(t.TempDir(), "tunnel.sock"))
	holder.logw = logs

	ln := &errListener{err: fmt.Errorf("accept tcp: too many open files")}
	entry := &tunnelEntry{
		egress:   "eg-accept",
		listen:   "127.0.0.1:0",
		restart:  tunRestartNo,
		listener: ln,
		conns:    make(map[net.Conn]struct{}),
		started:  time.Now(),
		logf:     holder.entryLogf("eg-accept"),
	}
	holder.mu.Lock()
	holder.byEgress["eg-accept"] = entry
	holder.mu.Unlock()

	go holder.serveEntry(entry)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		holder.mu.Lock()
		down := entry.listener == nil
		holder.mu.Unlock()
		if down {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	holder.mu.Lock()
	stillRegistered := holder.byEgress["eg-accept"] == entry
	state := entry.toState()
	holder.mu.Unlock()

	if !stillRegistered {
		t.Fatalf("entry was dropped from byEgress after an accept failure")
	}
	if state.State != "stopped" {
		t.Fatalf("entry still reports %q after accept failure, want stopped", state.State)
	}
	if !ln.closed {
		t.Fatalf("listener was not closed after the permanent accept failure")
	}
	if !strings.Contains(logs.String(), "[tunnel eg-accept] accept failed") {
		t.Fatalf("accept failure not logged via entry.logf; got:\n%s", logs.String())
	}
}

// TestTunnelHolder_RestoreBindFailureKeepsStoppedEntry 验证 restore 时 restart=always 重绑失败
// 不再直接丢弃条目:仍以 stopped 登记(ps 可见,relay start/restart 可重开)。
func TestTunnelHolder_RestoreBindFailureKeepsStoppedEntry(t *testing.T) {
	logs := &syncBuffer{}
	h := newTunnelHolder(context.Background(), nil, filepath.Join(t.TempDir(), "tunnel.sock"))
	h.logw = logs
	h.statePath = filepath.Join(t.TempDir(), "tunnels.json")
	started := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := persistTunnels(h.statePath, []tunnelState{
		{Egress: "eg-restore", Listen: nonBindableListen, Restart: tunRestartAlways, Started: started},
	}); err != nil {
		t.Fatalf("persist state: %v", err)
	}

	h.restore()

	h.mu.Lock()
	entry, ok := h.byEgress["eg-restore"]
	h.mu.Unlock()
	if !ok {
		t.Fatalf("failed restore dropped the entry from byEgress; logs:\n%s", logs.String())
	}
	if st := entry.toState(); st.State != "stopped" || st.Listen != nonBindableListen ||
		st.Restart != tunRestartAlways || !st.Started.Equal(started) {
		t.Fatalf("restored entry = %+v, want stopped with listen/restart/started preserved", st)
	}
}

// TestRemovePersistedTunnel 验证 node 未运行时的意图清理:删掉指定 egress、保留其余,幂等且原子写。
func TestRemovePersistedTunnel(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")

	// 文件不存在:没东西可删,不算错误。
	if removed, err := removePersistedTunnel(statePath, "eg-a"); err != nil || removed {
		t.Fatalf("removePersistedTunnel on missing file = (%v, %v), want (false, nil)", removed, err)
	}

	if err := persistTunnels(statePath, []tunnelState{
		{Egress: "eg-a", Listen: "127.0.0.1:1080", Restart: tunRestartAlways, State: "running"},
		{Egress: "eg-b", Listen: "127.0.0.1:1081", Restart: tunRestartNo, State: "stopped"},
	}); err != nil {
		t.Fatalf("persist state: %v", err)
	}

	removed, err := removePersistedTunnel(statePath, "eg-a")
	if err != nil || !removed {
		t.Fatalf("removePersistedTunnel = (%v, %v), want (true, nil)", removed, err)
	}
	states, err := loadTunnels(statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	if len(states) != 1 || states[0].Egress != "eg-b" {
		t.Fatalf("state after removal = %+v, want only eg-b", states)
	}

	// 目标不存在:返回 false 且不动文件。
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	removed, err = removePersistedTunnel(statePath, "eg-nope")
	if err != nil || removed {
		t.Fatalf("removePersistedTunnel(eg-nope) = (%v, %v), want (false, nil)", removed, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state again: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("state file rewritten for a no-op removal:\nbefore=%s\nafter=%s", before, after)
	}
}
