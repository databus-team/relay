package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/protocol"
)

func forwardSockPath() string { return daemon.SockFile("forward") }

func forwardStatePath() string {
	return filepath.Join(filepath.Dir(daemon.SockFile("forward")), "forwards.json")
}

// fwdIntent 是一条任务绑定转发的持久化形态:转发挂在 detach 任务的 task_id 下,
// 生命周期跟随任务(register/unregister)。
type fwdIntent struct {
	TaskID   string            `json:"task_id"`
	Executor string            `json:"executor"`
	Specs    []PortForwardSpec `json:"specs"`
}

// forwardState 是一条独立转发(一等对象)的序列化意图 + 运行时状态。
// 身份是 local_port;Spec 内嵌完整规格(bind/local/remote),restore 后据此重开监听与隧道。
type forwardState struct {
	Port     int             `json:"id"`       // local_port,对象身份
	Executor string          `json:"executor"` // 出网点 egress
	Spec     PortForwardSpec `json:"spec"`
	Restart  string          `json:"restart"`
	State    string          `json:"state"` // running|stopped
	Started  time.Time       `json:"started,omitempty"`
}

// forwardStateFile 是 forwards.json 的持久化形态:任务绑定转发与独立转发两份列表。
// 顶层是数组的旧文件按 fwdIntent 读(兼容),persist 一律写对象格式。
type forwardStateFile struct {
	Tasks    []fwdIntent    `json:"tasks"`
	Forwards []forwardState `json:"forwards"`
}

// 控制 socket 的操作码常量:避免 register/unregister/open/close/restart/list 以裸字符串多处出现。
const (
	opRegister   = "register"
	opUnregister = "unregister"
	opOpen       = "open"
	opClose      = "close"
	opRestart    = "restart"
	opList       = "list"

	// 持久化失败时给 CLI 的告警尾句:说明「意图没落盘」对用户的实际后果(见 persistWarnLocked)。
	fwdPersistLostOnRestart = "the forward will not be restored on the next node start"
	fwdPersistLostOnClose   = "the persisted intent was not removed and the forward will reappear on the next node start"
)

// fwdControlMsg 是一条来自 CLI 的控制消息(JSON 单行,如 docker CLI -> dockerd unix socket)。
// register: 为 task_id 注册一组端口转发;unregister: 撤销该 task 全部转发;
// open: 创建/幂等重开一条独立转发;close: 关闭并撤销;restart: 关闭后按原参数重开;list: 列出全部独立转发。
type fwdControlMsg struct {
	Op       string            `json:"op"`
	TaskID   string            `json:"task_id,omitempty"`
	Executor string            `json:"executor,omitempty"`
	Specs    []PortForwardSpec `json:"specs,omitempty"`
	Port     int               `json:"port,omitempty"`
	Spec     *PortForwardSpec  `json:"spec,omitempty"`
	Restart  string            `json:"restart,omitempty"`
}

// fwdControlResp 是对控制消息的应答(JSON 单行)。
// Registered/Rejected 是 register 时逐条规格的结果:Rejected 为端口冲突等未能绑定(视为告警而非致命)。
// Warning 承载「操作已生效但持久化失败」这类非致命问题(OK 仍为 true);Forwards 供 list 返回全部独立转发。
type fwdControlResp struct {
	OK         bool          `json:"ok"`
	Error      string        `json:"error,omitempty"`
	Warning    string        `json:"warning,omitempty"`
	Registered []string      `json:"registered,omitempty"`
	Rejected   []string      `json:"rejected,omitempty"`
	Forwards   []forwardState `json:"forwards,omitempty"`
}

// fwdForwarder 是一个活跃端口转发:监听本地端口,每条连接经中转隧道转发到远端目标。
// addr 为注册时的 "bind:port",供 byAddr 冲突检测与 unregister 精确删除复用。
// id != 0 表示独立转发(一等对象,进 byID);id == 0 是任务绑定转发(taskID 非空,进 byTask)。
// closed 标记「条目已被主动关闭」,供 serveForwarder 区分主动 Close 与真实 Accept 失败。
type fwdForwarder struct {
	id       int // local_port;0 = 任务绑定转发
	taskID   string
	executor string
	addr     string // canonical "bind:port"
	spec     PortForwardSpec
	restart  string
	listener net.Listener
	started  time.Time
	closed   bool

	logf func(format string, args ...any) // 连接级错误经此路由到日志目标,前缀 [forward <port>]
}

// entryLogf 返回带 `[forward <port>]` 前缀的日志器;relay logs 的回退据此前缀过滤该转发的行。
func (h *forwardHolder) entryLogf(port int) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(h.logw, "[forward "+strconv.Itoa(port)+"] "+format+"\n", args...)
	}
}

// toState 把内存转发器投影为可序列化的 forwardState,state 由监听器是否存活决定。
func (e *fwdForwarder) toState() forwardState {
	state := "stopped"
	if e.listener != nil {
		state = "running"
	}
	return forwardState{
		Port:     e.id,
		Executor: e.executor,
		Spec:     e.spec,
		Restart:  e.restart,
		State:    state,
		Started:  e.started,
	}
}

type forwardHolder struct {
	rb        *relaybackend.RelayBackend
	ctx       context.Context
	sockPath  string
	statePath string
	logw      io.Writer // 日志目标,默认 os.Stderr;可注入以便单测无竞态捕获

	mu     sync.Mutex
	byAddr map[string]*fwdForwarder // 冲突检测域:任务绑定 + 独立转发共用
	byTask map[string][]*fwdForwarder
	byID   map[int]*fwdForwarder // 独立转发(一等对象),key = local_port
}

func newForwardHolder(ctx context.Context, rb *relaybackend.RelayBackend, sockPath string) *forwardHolder {
	statePath := forwardStatePath()
	if sockPath != "" {
		statePath = filepath.Join(filepath.Dir(sockPath), "forwards.json")
	} else {
		sockPath = forwardSockPath()
	}
	return &forwardHolder{
		rb:        rb,
		ctx:       ctx,
		sockPath:  sockPath,
		statePath: statePath,
		logw:      os.Stderr,
		byAddr:    make(map[string]*fwdForwarder),
		byTask:    make(map[string][]*fwdForwarder),
		byID:      make(map[int]*fwdForwarder),
	}
}

func (h *forwardHolder) Run() error {
	ln, err := listenHolderSocket(h.sockPath)
	if err != nil {
		return fmt.Errorf("listen forward control socket %s: %w", h.sockPath, err)
	}
	defer os.Remove(h.sockPath)

	fmt.Printf("[Node] Port-forward holder listening on unix socket %s\n", h.sockPath)

	h.restore()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if h.ctx.Err() != nil {
				return nil
			}
			return err
		}
		go h.handleConn(conn)
	}
}

// handleConn 逐条处理一个控制连接上的 JSON 新行消息,直至对端关闭。
func (h *forwardHolder) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	for {
		var msg fwdControlMsg
		if err := dec.Decode(&msg); err != nil {
			return
		}
		resp := h.dispatch(msg)
		_ = enc.Encode(resp)
	}
}

func (h *forwardHolder) dispatch(msg fwdControlMsg) fwdControlResp {
	switch msg.Op {
	case opRegister:
		return h.opRegister(msg)
	case opUnregister:
		return h.opUnregister(msg)
	case opOpen:
		return h.opOpen(msg)
	case opClose:
		return h.opClose(msg)
	case opRestart:
		return h.opRestart(msg)
	case opList:
		return h.opList(msg)
	default:
		return fwdControlResp{OK: false, Error: fmt.Sprintf("unknown op %q", msg.Op)}
	}
}

func (h *forwardHolder) opRegister(msg fwdControlMsg) fwdControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()

	resp := fwdControlResp{OK: true}
	for _, spec := range msg.Specs {
		if spec.BindHost == "" {
			spec.BindHost = "127.0.0.1"
		}
		addr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
		if fw, exists := h.byAddr[addr]; exists {
			if fw.taskID == msg.TaskID {
				// 同任务复用:保留现有监听(不断在途连接),不视为冲突(R2/R13)。
				resp.Registered = append(resp.Registered, addr)
				continue
			}
			resp.Rejected = append(resp.Rejected, fmt.Sprintf("%s already held by another forwarder", addr))
			continue
		}
		if !isLoopbackAddr(spec.BindHost) {
			fmt.Fprintf(h.logw, "WARNING: listening on %s without authentication\n", addr)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			resp.Rejected = append(resp.Rejected, fmt.Sprintf("listen %s failed: %v", addr, err))
			continue
		}
		fw := &fwdForwarder{taskID: msg.TaskID, executor: msg.Executor, addr: addr, spec: spec, listener: ln, restart: tunRestartNo, started: time.Now(), logf: h.entryLogf(spec.LocalPort)}
		h.byAddr[addr] = fw
		h.byTask[msg.TaskID] = append(h.byTask[msg.TaskID], fw)
		resp.Registered = append(resp.Registered, addr)
		go h.serveForwarder(fw)
	}
	if len(resp.Registered) > 0 {
		if err := h.persistLocked(); err != nil {
			fmt.Fprintf(h.logw, "[port-forward] persist state: %v\n", err)
		}
	}
	return resp
}

func (h *forwardHolder) opUnregister(msg fwdControlMsg) fwdControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()

	resp := fwdControlResp{OK: true}
	for _, fw := range h.byTask[msg.TaskID] {
		delete(h.byAddr, fw.addr)
		_ = fw.listener.Close()
	}
	delete(h.byTask, msg.TaskID)
	if err := h.persistLocked(); err != nil {
		fmt.Fprintf(h.logw, "[port-forward] persist state: %v\n", err)
	}
	return resp
}

// opOpen 创建或幂等重开一条独立转发。同 port 已存在时:监听地址未变则复用现有监听
// (只更新 restart/executor,不断在途连接);地址变化走「先绑新监听、成功再拆旧」的原子替换,
// 绑定失败时既有条目原样保留(对齐 tunnel 的 openEntryLocked)。
// 端口被任务绑定转发占用时按冲突拒绝。
func (h *forwardHolder) opOpen(msg fwdControlMsg) fwdControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()

	if msg.Spec == nil {
		return fwdControlResp{OK: false, Error: "spec must not be empty"}
	}
	if msg.Executor == "" {
		return fwdControlResp{OK: false, Error: "executor must not be empty"}
	}
	if msg.Port <= 0 || msg.Port > 65535 {
		return fwdControlResp{OK: false, Error: fmt.Sprintf("invalid port %d", msg.Port)}
	}
	spec := *msg.Spec
	if spec.BindHost == "" {
		spec.BindHost = "127.0.0.1"
	}
	spec.LocalPort = msg.Port // 身份以 port 为准,避免调用方在 spec 里填了不一致的 local_port
	addr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)

	if prev, ok := h.byID[msg.Port]; ok {
		if prev.listener != nil && prev.addr == addr {
			prev.restart = normalizeTunnelRestart(msg.Restart)
			prev.executor = msg.Executor
			return fwdControlResp{OK: true, Warning: h.persistWarnLocked(msg.Port, fwdPersistLostOnRestart)}
		}
		if err := h.openReplaceLocked(prev, spec, addr, msg.Executor, msg.Restart); err != nil {
			return fwdControlResp{OK: false, Error: err.Error()}
		}
		return fwdControlResp{OK: true, Warning: h.persistWarnLocked(msg.Port, fwdPersistLostOnRestart)}
	}

	if fw, exists := h.byAddr[addr]; exists {
		return fwdControlResp{OK: false, Error: fmt.Sprintf("port %d already held by another forwarder (task %s)", msg.Port, orDash(fw.taskID))}
	}

	fw, err := h.bindForwardLocked(msg.Port, spec, addr, msg.Executor, msg.Restart)
	if err != nil {
		return fwdControlResp{OK: false, Error: err.Error()}
	}
	h.installForwardLocked(fw)
	return fwdControlResp{OK: true, Warning: h.persistWarnLocked(msg.Port, fwdPersistLostOnRestart)}
}

// openReplaceLocked 用新 spec 替换已有独立转发的监听:先绑定新监听,成功后才拆除旧的。
// 新监听地址若已被其他转发(含任务绑定)占用则绑定前先报错,旧条目毫发无损。调用方须已持有 h.mu。
func (h *forwardHolder) openReplaceLocked(prev *fwdForwarder, spec PortForwardSpec, addr, executor, restart string) error {
	if fw, exists := h.byAddr[addr]; exists && fw != prev {
		return fmt.Errorf("listen %s failed: already held by another forwarder", addr)
	}
	fw, err := h.bindForwardLocked(prev.id, spec, addr, executor, restart)
	if err != nil {
		return err
	}
	delete(h.byAddr, prev.addr)
	_ = prev.listener.Close()
	h.installForwardLocked(fw)
	return nil
}

// opClose 关闭并移除指定端口的独立转发(docker rm 语义):关闭监听、从 byAddr/byID 删除、
// 并移除持久化意图。任务绑定转发的移除走 opUnregister,不经这里。
func (h *forwardHolder) opClose(msg fwdControlMsg) fwdControlResp {
	if msg.Port <= 0 {
		return fwdControlResp{OK: false, Error: "port must be positive"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	fw, ok := h.byID[msg.Port]
	if !ok {
		return fwdControlResp{OK: false, Error: fmt.Sprintf("no forward for port %d", msg.Port)}
	}
	delete(h.byAddr, fw.addr)
	delete(h.byID, msg.Port)
	_ = fw.listener.Close()
	return fwdControlResp{OK: true, Warning: h.persistWarnLocked(msg.Port, fwdPersistLostOnClose)}
}

// opRestart 关闭现有监听,再按同一 spec/executor/restart 重开,保留持久化意图。
// 条目不存在时报错;重开绑定失败时条目留为 stopped(意图文件保持原样)。
func (h *forwardHolder) opRestart(msg fwdControlMsg) fwdControlResp {
	if msg.Port <= 0 {
		return fwdControlResp{OK: false, Error: "port must be positive"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	fw, ok := h.byID[msg.Port]
	if !ok {
		return fwdControlResp{OK: false, Error: fmt.Sprintf("no forward for port %d", msg.Port)}
	}
	delete(h.byAddr, fw.addr)
	_ = fw.listener.Close()
	fw.listener = nil // 重开失败时 toState 据此降为 stopped
	newFw, err := h.bindForwardLocked(fw.id, fw.spec, fw.addr, fw.executor, fw.restart)
	if err != nil {
		return fwdControlResp{OK: false, Error: err.Error()}
	}
	h.installForwardLocked(newFw)
	return fwdControlResp{OK: true, Warning: h.persistWarnLocked(msg.Port, fwdPersistLostOnRestart)}
}

// opList 返回全部独立转发条目(依据监听器是否存活区分 running/stopped)。
// 任务绑定转发不出行——它已在任务行的 PORTS 列展示。
func (h *forwardHolder) opList(msg fwdControlMsg) fwdControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fwdControlResp{OK: true, Forwards: h.standaloneSnapshotLocked()}
}

// bindForwardLocked 绑定监听并构造转发器(不登记)。非 loopback 绑定先发出告警;
// 绑定失败时调用方的既有条目毫发无损。调用方须已持有 h.mu。
func (h *forwardHolder) bindForwardLocked(id int, spec PortForwardSpec, addr, executor, restart string) (*fwdForwarder, error) {
	if !isLoopbackAddr(spec.BindHost) {
		fmt.Fprintf(h.logw, "WARNING: listening on %s without authentication\n", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s failed: %w", addr, err)
	}
	return &fwdForwarder{
		id:       id,
		executor: executor,
		addr:     addr,
		spec:     spec,
		restart:  normalizeTunnelRestart(restart),
		listener: ln,
		started:  time.Now(),
		logf:     h.entryLogf(spec.LocalPort),
	}, nil
}

// installForwardLocked 把已绑定的转发器登记进 byAddr(+独立转发进 byID)并启动 accept 循环。
// accept 循环不触碰 h.mu,可在锁内安全启动。调用方须已持有 h.mu。
func (h *forwardHolder) installForwardLocked(fw *fwdForwarder) {
	if fw.id != 0 {
		h.byID[fw.id] = fw
	}
	h.byAddr[fw.addr] = fw
	go h.serveForwarder(fw)
}

// registerStoppedLocked 就地登记一条 stopped 独立转发(不绑定监听),保留 spec/executor/restart/started,
// 使其在 list/ps 可见,并可被 `relay start <port>` / `relay restart <port>` 依原参数重开。调用方须已持有 h.mu。
func (h *forwardHolder) registerStoppedLocked(st forwardState) {
	fw := &fwdForwarder{
		id:       st.Port,
		executor: st.Executor,
		spec:     st.Spec,
		restart:  st.Restart,
		started:  st.Started,
		logf:     h.entryLogf(st.Port),
	}
	if st.Spec.BindHost != "" {
		fw.addr = fmt.Sprintf("%s:%d", st.Spec.BindHost, st.Spec.LocalPort)
	}
	h.byID[st.Port] = fw
}

// serveForwarder 接收来自本地监听端口的连接,并逐条建立中转隧道。
// Accept 失败分两类:主动 close(closed)正常退出;其他失败(EMFILE/ENOBUFS 等)必须留痕并把条目
// 降为 stopped——否则 accept 循环已死、listener 仍在,list/ps 会一直谎报 running(对齐 tunnel serveEntry)。
// 任务绑定转发(id==0)不进 byID,其状态由任务行承载,这里保持原行为的静默退出。
func (h *forwardHolder) serveForwarder(fw *fwdForwarder) {
	ln := fw.listener
	for {
		conn, err := ln.Accept()
		if err != nil {
			h.mu.Lock()
			unexpected := !fw.closed && fw.id != 0 && h.byID[fw.id] == fw && fw.listener == ln
			if unexpected {
				fw.closed = true
				_ = fw.listener.Close()
				fw.listener = nil // toState 由此降为 stopped
			}
			h.mu.Unlock()
			if unexpected {
				fw.logf("accept failed: %v; forward stopped (restart it with `relay restart %d`)", err, fw.id)
			}
			return
		}
		go h.pumpFwdConn(fw, conn)
	}
}

// pumpFwdConn 复用 handlePortForwardConn 的双向泵:本地 TCP -> TunnelStream -> 远端目标。
// 连接级错误经 fw.logf 路由到 `[forward <port>]` 前缀(relay logs <port> 据此过滤)。
func (h *forwardHolder) pumpFwdConn(fw *fwdForwarder, conn net.Conn) {
	handlePortForwardConn(h.ctx, h.rb, fw.executor, &fw.spec, conn, fw.logf)
}

// ensureLocalRelayNode 确保本地 node daemon 处于运行状态(未运行则启动),
// 并等待其控制 socket(sockPath:端口转发 holder 的 forward.sock,或隧道 holder 的 tunnel.sock)
// 就绪——以成功拨号为准,而非仅存在文件。CLI 侧 `relay run -d -p`、`relay run -t tunnel` 与
// `relay run -t forward` 前调用。holder 只在 node 的 agent 角色块内启动,因此:
//   - node 未运行且本配置不能归一化为 agent 角色(显式 roles:[hub])时,自动拉起只会有/无 holder,
//     直接给出可操作错误而非启动一个无用的纯 hub node;
//   - node 已在跑(可能用另一份配置)时不额外校验本 CLI 配置的角色,只等 socket 就绪。
func ensureLocalRelayNode(cfg *config.Config, sockPath string) error {
	if sockPath == "" {
		sockPath = forwardSockPath()
	}
	st := daemon.Get(daemon.PidFile("node"))
	if st.Err != nil {
		return st.Err
	}
	if !st.Running {
		// 需要自动拉起 node;用 CLI 的 -c 配置启动,该配置必须能归一化为 agent 角色,否则 holder 起不来。
		if !cfg.IsAgent() {
			return fmt.Errorf("local node is not running and this config does not resolve to the agent role; add `node.roles: [agent]` (or omit roles to default to agent), or start a node daemon first (`relay node start`)")
		}
		if err := daemonStart("node"); err != nil {
			return err
		}
	}
	// 以 net.Dial 成功作为 socket 就绪信号;node 在跑但非 agent(纯 hub)时不会有该 socket,超时并给出指引。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sockPath, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("local node control socket %s did not become ready (is a node with node.roles: [agent] running?)", sockPath)
}

// captureDetachTaskID 处理一条 detach 执行的 stdout chunk,解析出任务 ID(带 marker 前缀)。
// 命中标记行时返回 (id, ok=true),调用方应抑制该行不回显(CLI 在 detach 结束处统一 echo)。
// 纯函数,便于单测。
func captureDetachTaskID(data string) (id string, ok bool) {
	if c, ok := strings.CutPrefix(data, protocol.ForwardTaskIDMarker); ok {
		if c = strings.TrimSpace(c); c != "" {
			return c, true
		}
	}
	return "", false
}

// sendForwardControl 经 unix socket 发送一条控制消息并解码应答。调用方负责构造 msg 与解读应答。
// conn 在返回前关闭。底层的 socket 传输错误统一返回,让上层决定是否致命。
func sendForwardControl(sockPath string, msg fwdControlMsg) (fwdControlResp, error) {
	if sockPath == "" {
		sockPath = forwardSockPath()
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fwdControlResp{}, fmt.Errorf("connect forward control socket %s: %w", sockPath, err)
	}
	defer conn.Close()

	// 整个控制 socket round-trip 必须有界。仅 DialTimeout 只限制了连接阶段,
	// node 接受连接后若 handler 卡死,Encode/Decode 会让 CLI 无限阻塞。设定读写 deadline 兜底。
	timeout := 5 * time.Second
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fwdControlResp{}, fmt.Errorf("set forward control socket deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return fwdControlResp{}, fmt.Errorf("send %s: %w", msg.Op, err)
	}
	var resp fwdControlResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fwdControlResp{}, fmt.Errorf("read %s response: %w", msg.Op, err)
	}
	return resp, nil
}

// sendForwardRegister 通过 unix socket 向本地 node 发送 register 控制消息,并返回逐条结果。
// 仅当 socket 传输失败或 node 拒绝整个请求时才返回 err;规格级端口冲突(Rejected)作为告警逐条返回,不中断。
func sendForwardRegister(sockPath, taskID, executor string, specs []*PortForwardSpec) (registered, rejected []string, err error) {
	msg := fwdControlMsg{Op: opRegister, TaskID: taskID, Executor: executor}
	for _, spec := range specs {
		msg.Specs = append(msg.Specs, *spec)
	}
	resp, err := sendForwardControl(sockPath, msg)
	if err != nil {
		return nil, nil, err
	}
	if !resp.OK {
		return resp.Registered, resp.Rejected, fmt.Errorf("register rejected: %s", resp.Error)
	}
	return resp.Registered, resp.Rejected, nil
}

// sendForwardUnregister 通过 unix socket 向本地 node 发送 unregister 控制消息,撤销指定 task_id 的全部转发。
// 返回错误仅当 socket 传输失败或 node 拒绝;软错误由调用方决定是否告警。
func sendForwardUnregister(sockPath, taskID string) error {
	resp, err := sendForwardControl(sockPath, fwdControlMsg{Op: opUnregister, TaskID: taskID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("unregister rejected: %s", resp.Error)
	}
	return nil
}

// snapshotLocked 把当前全部记录投影为 forwardStateFile:byTask → tasks,byID → forwards。
// 每个 task 取其第一条 forwarder 的 executor(同任务的 forwarder executor 必然相同),
// 收集该任务下全部活跃规格。调用方须已持有 h.mu。
func (h *forwardHolder) snapshotLocked() forwardStateFile {
	file := forwardStateFile{
		Tasks:    make([]fwdIntent, 0, len(h.byTask)),
		Forwards: make([]forwardState, 0, len(h.byID)),
	}
	for taskID, fws := range h.byTask {
		if len(fws) == 0 {
			continue
		}
		specs := make([]PortForwardSpec, 0, len(fws))
		for _, fw := range fws {
			specs = append(specs, fw.spec)
		}
		file.Tasks = append(file.Tasks, fwdIntent{
			TaskID:   taskID,
			Executor: fws[0].executor,
			Specs:    specs,
		})
	}
	for _, fw := range h.byID {
		file.Forwards = append(file.Forwards, fw.toState())
	}
	return file
}

// standaloneSnapshotLocked 返回全部独立转发条目(供 opList)。调用方须已持有 h.mu。
func (h *forwardHolder) standaloneSnapshotLocked() []forwardState {
	states := make([]forwardState, 0, len(h.byID))
	for _, fw := range h.byID {
		states = append(states, fw.toState())
	}
	return states
}

// persistWarnLocked 落盘当前意图;失败时记日志并返回给 CLI 的 Warning 文案(空串表示成功)。
// 持久化失败不算操作失败——监听器已经变更,故 OK 仍为 true,但必须让 CLI 能把它打到 stderr。
// 调用方须已持有 h.mu。
func (h *forwardHolder) persistWarnLocked(port int, consequence string) string {
	if err := h.persistLocked(); err != nil {
		fmt.Fprintf(h.logw, "[forward %d] persist state: %v\n", port, err)
		return fmt.Sprintf("state not persisted: %v; %s", err, consequence)
	}
	return ""
}

func (h *forwardHolder) persistLocked() error {
	return persistForwards(h.statePath, h.snapshotLocked())
}

// loadForwards 读取持久化的转发状态;文件不存在时返回空文件(不视为错误)。
// 兼容旧格式:顶层是数组(历史 fwdIntent 列表)时全部按任务绑定转发读入。
func loadForwards(statePath string) (forwardStateFile, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return forwardStateFile{}, nil
		}
		return forwardStateFile{}, err
	}
	var file forwardStateFile
	if err := json.Unmarshal(data, &file); err == nil {
		return file, nil
	}
	// 旧格式:顶层数组 → 全当任务绑定转发。
	var intents []fwdIntent
	if err := json.Unmarshal(data, &intents); err == nil {
		return forwardStateFile{Tasks: intents}, nil
	}
	return forwardStateFile{}, fmt.Errorf("parse forward state: unsupported format")
}

// persistForwards 原子地把转发状态写回 statePath:先写 `statePath+.tmp` 再 rename(与 config 原子写一致)。
func persistForwards(statePath string, file forwardStateFile) error {
	if dir := filepath.Dir(statePath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir state dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmpPath, statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic replace state: %w", err)
	}
	return nil
}

// restore 在启动时重放持久化的转发状态:任务绑定转发按 register 恢复;独立转发按 restart 策略
// 重新绑定(restart=always)或就地标记为 stopped(restart=no,不绑定)。单条失败仅记录并继续,
// 但失败条目仍以 stopped 形态登记,避免「意图还在盘上、内存里却查无此转发」。
func (h *forwardHolder) restore() {
	file, err := loadForwards(h.statePath)
	if err != nil {
		fmt.Fprintf(h.logw, "[Node] load forward state: %v\n", err)
		return
	}

	// 任务绑定转发:register 自己加锁。
	for _, intent := range file.Tasks {
		if intent.TaskID == "" || len(intent.Specs) == 0 {
			continue
		}
		resp := h.opRegister(fwdControlMsg{
			Op:       opRegister,
			TaskID:   intent.TaskID,
			Executor: intent.Executor,
			Specs:    intent.Specs,
		})
		for _, addr := range resp.Registered {
			fmt.Fprintf(h.logw, "[port-forward] restored %s (task %s)\n", addr, intent.TaskID)
		}
		for _, r := range resp.Rejected {
			fmt.Fprintf(h.logw, "[port-forward] restore failed: %s (task %s)\n", r, intent.TaskID)
		}
	}

	// 独立转发:按 restart 策略重开或留 stopped。
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range file.Forwards {
		if st.Port <= 0 || st.Spec.LocalPort == 0 {
			continue
		}
		if st.Restart == tunRestartAlways {
			if err := h.openForwardStateLocked(st); err != nil {
				fmt.Fprintf(h.logw, "[forward %d] open failed: %v; kept as stopped\n", st.Port, err)
				h.registerStoppedLocked(st)
				continue
			}
			fmt.Fprintf(h.logw, "[forward %d] restored (running)\n", st.Port)
		} else {
			h.registerStoppedLocked(st)
			fmt.Fprintf(h.logw, "[forward %d] restored (stopped)\n", st.Port)
		}
	}
}

// openForwardStateLocked 按 forwardState 重新绑定监听并登记(restore 的 restart=always 分支)。
// 调用方须已持有 h.mu。
func (h *forwardHolder) openForwardStateLocked(st forwardState) error {
	if st.Executor == "" {
		return fmt.Errorf("executor empty")
	}
	spec := st.Spec
	if spec.BindHost == "" {
		spec.BindHost = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
	fw, err := h.bindForwardLocked(st.Port, spec, addr, st.Executor, st.Restart)
	if err != nil {
		return err
	}
	h.installForwardLocked(fw)
	return nil
}

// removePersistedForward 从持久化状态里移除指定端口的独立转发条目(holder 不可达时的清理路径,
// 如 node 未运行)。只从 forwards 列表删,不碰 tasks——任务绑定转发的移除走 unregister 整 task。
// 返回是否确有条目被移除;文件不存在或本就没有该条目时返回 (false, nil)。
func removePersistedForward(statePath string, port int) (bool, error) {
	file, err := loadForwards(statePath)
	if err != nil {
		return false, err
	}
	kept := make([]forwardState, 0, len(file.Forwards))
	removed := false
	for _, st := range file.Forwards {
		if st.Port == port {
			removed = true
			continue
		}
		kept = append(kept, st)
	}
	if !removed {
		return false, nil
	}
	file.Forwards = kept
	if err := persistForwards(statePath, file); err != nil {
		return false, err
	}
	return true, nil
}

// filterForwards 按 target 精确过滤 executor(空 target = 不过滤);all=false 时仅保留 running。
// 只有 relay ps 使用它:relay stop/start/logs <port> 走 resolveLifecycle/forwardStatesForRead,不经这里。
func filterForwards(states []forwardState, target string, all bool) []forwardState {
	out := make([]forwardState, 0, len(states))
	for _, st := range states {
		if target != "" && st.Executor != target {
			continue
		}
		if !all && st.State != "running" {
			continue
		}
		out = append(out, st)
	}
	return out
}
