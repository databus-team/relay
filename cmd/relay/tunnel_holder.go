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
	"sync"
	"time"

	"github.com/user/relay/internal/daemon"
	relaybackend "github.com/user/relay/internal/relay/backend"
)

// tunnelSockPath 返回本地 node 隧道控制 socket 的绝对路径 `~/.relay/tunnel.sock`。
// 复用 daemon.SockFile 与 node.pid/node.log 共用同一 baseDir,避免路径分叉。
func tunnelSockPath() string { return daemon.SockFile("tunnel") }

// relayBaseDir 返回 node daemon 的 baseDir `~/.relay`(与 daemon.baseDir 一致,但其未导出)。
func relayBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".relay")
}

// tunnelStatePath 返回隧道意图持久化文件 `~/.relay/tunnels.json`。
func tunnelStatePath() string { return filepath.Join(relayBaseDir(), "tunnels.json") }

// 隧道控制 socket 的操作码常量 + 重启策略常量:避免裸字符串在多处出现。
const (
	tunOpOpen    = "open"
	tunOpClose   = "close"
	tunOpRestart = "restart"
	tunOpList    = "list"

	tunRestartAlways = "always"
	tunRestartNo     = "no"
)

// tunControlMsg 是一条来自 CLI 的控制消息(JSON 单行,如 docker CLI -> dockerd unix socket)。
// open: 打开(或重启)一条隧道;close: 关闭并撤销(rm);restart: 关闭后按原参数重开;list: 列出全部。
type tunControlMsg struct {
	Op      string `json:"op"`
	Egress  string `json:"egress,omitempty"`
	Listen  string `json:"listen,omitempty"`
	Restart string `json:"restart,omitempty"`
}

// tunControlResp 是对控制消息的应答(JSON 单行)。Tunnels 供 list 返回全部隧道现状。
type tunControlResp struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Tunnels []tunnelState `json:"tunnels,omitempty"`
}

// tunnelState 是一条隧道的序列化意图 + 运行时状态。这就是 list 返回、ps 合并的形态。
type tunnelState struct {
	Egress  string    `json:"egress"`
	Listen  string    `json:"listen"`
	Restart string    `json:"restart"`
	State   string    `json:"state"` // running|stopped
	Started time.Time `json:"started,omitempty"`
}

// tunnelEntry 是常驻 node 进程内一条活跃隧道(内存态)。conns 记录在途的已被 Accept 的连接,
// 供关闭时连同监听器一起回收。设计上 holder 拥有唯一一把 mu,entry 自身不加锁。
type tunnelEntry struct {
	egress   string
	listen   string
	restart  string
	listener net.Listener
	conns    map[net.Conn]struct{}
	state    string
	started  time.Time

	logf func(format string, args ...any) // 连接级错误经此路由到调用方的日志器,前缀 [tunnel <egress>]
}

// toState 把内存条目投影为可序列化的 tunnelState,state 由监听器是否存活决定。
func (e *tunnelEntry) toState() tunnelState {
	state := "stopped"
	if e.listener != nil {
		state = "running"
	}
	return tunnelState{
		Egress:  e.egress,
		Listen:  e.listen,
		Restart: e.restart,
		State:   state,
		Started: e.started,
	}
}

// tunnelHolder 是常驻于本地 node(agent 角色)进程内的 SOCKS5 隧道持有者,类比 dockerd。
// 它通过 unix socket 接受 CLI 的 open/close/restart/list 控制,并负责维持各隧道的本地监听;
// 即使发起 `relay tunnel` 的 CLI 已退出,隧道依然存活。意图持久化到 tunnels.json,重启后按 restart 策略恢复。
type tunnelHolder struct {
	rb        *relaybackend.RelayBackend
	ctx       context.Context
	sockPath  string
	statePath string
	logw      io.Writer // 日志目标,默认 os.Stderr;可注入以便单测无竞态捕获

	mu       sync.Mutex
	byEgress map[string]*tunnelEntry
}

// newTunnelHolder 构造一个隧道持有者。sockPath 为空时使用默认 `~/.relay/tunnel.sock`。
// statePath 默认为 `~/.relay/tunnels.json`(测试可覆写以隔离)。
func newTunnelHolder(ctx context.Context, rb *relaybackend.RelayBackend, sockPath string) *tunnelHolder {
	if sockPath == "" {
		sockPath = tunnelSockPath()
	}
	return &tunnelHolder{
		rb:        rb,
		ctx:       ctx,
		sockPath:  sockPath,
		statePath: tunnelStatePath(),
		logw:      os.Stderr,
		byEgress:  make(map[string]*tunnelEntry),
	}
}

// Run 监听 unix socket 并处理控制消息,启动时先恢复持久化的隧道意图,直到构造时的 ctx 取消或 Accept 出错。
// 返回时 socket 已清理。
func (h *tunnelHolder) Run() error {
	_ = os.Remove(h.sockPath)
	if dir := filepath.Dir(h.sockPath); dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	ln, err := net.Listen("unix", h.sockPath)
	if err != nil {
		return fmt.Errorf("listen tunnel control socket %s: %w", h.sockPath, err)
	}
	defer os.Remove(h.sockPath)

	fmt.Printf("[Node] Tunnel holder listening on unix socket %s\n", h.sockPath)

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
func (h *tunnelHolder) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	for {
		var msg tunControlMsg
		if err := dec.Decode(&msg); err != nil {
			return
		}
		resp := h.dispatch(msg)
		_ = enc.Encode(resp)
	}
}

func (h *tunnelHolder) dispatch(msg tunControlMsg) tunControlResp {
	switch msg.Op {
	case tunOpOpen:
		return h.opOpen(msg)
	case tunOpClose:
		return h.opClose(msg)
	case tunOpRestart:
		return h.opRestart(msg)
	case tunOpList:
		return h.opList(msg)
	default:
		return tunControlResp{OK: false, Error: fmt.Sprintf("unknown op %q", msg.Op)}
	}
}

// closeEntryLocked 关闭条目的监听器与全部在途连接并标记为 stopped。调用方须已持有 h.mu。
func (h *tunnelHolder) closeEntryLocked(entry *tunnelEntry) {
	if entry.listener != nil {
		_ = entry.listener.Close()
		entry.listener = nil
	}
	for conn := range entry.conns {
		_ = conn.Close()
	}
	entry.conns = make(map[net.Conn]struct{})
	entry.state = "stopped"
}

// openEntryLocked 绑定一条隧道:非 loopback 绑定发出与前台 tunnel 一致的告警,监听成功后
// 成为 running 并启动 accept 循环。调用方须已持有 h.mu。
func (h *tunnelHolder) openEntryLocked(egress, listen, restart string) error {
	if !isLoopbackAddr(listen) {
		fmt.Fprintf(h.logw, "WARNING: listening on %q (non-loopback). This tunnel has no auth; any host that can reach this port can use it to reach the executor's network.\n", listen)
	}
	if restart == "" {
		restart = tunRestartNo
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s failed: %w", listen, err)
	}
	entry := &tunnelEntry{
		egress:   egress,
		listen:   listen,
		restart:  restart,
		listener: ln,
		conns:    make(map[net.Conn]struct{}),
		state:    "running",
		started:  time.Now(),
		logf: func(format string, args ...any) {
			fmt.Fprintf(h.logw, "[tunnel "+egress+"] "+format+"\n", args...)
		},
	}
	h.byEgress[egress] = entry
	go h.serveEntry(entry) // accept 循环不触碰 h.mu,可在锁内安全启动
	return nil
}

// opOpen 打开(或幂等重开)一条隧道。若该 egress 已存在条目,则关闭并重开,保持持久化意图。
func (h *tunnelHolder) opOpen(msg tunControlMsg) tunControlResp {
	if msg.Egress == "" {
		return tunControlResp{OK: false, Error: "egress must not be empty"}
	}
	if msg.Listen == "" {
		return tunControlResp{OK: false, Error: "listen must not be empty"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 幂等:重复 open 同一 egress 先关闭旧监听,再按新参数重开,保持持久化意图。
	if entry, ok := h.byEgress[msg.Egress]; ok {
		h.closeEntryLocked(entry)
		delete(h.byEgress, msg.Egress)
	}

	if err := h.openEntryLocked(msg.Egress, msg.Listen, msg.Restart); err != nil {
		return tunControlResp{OK: false, Error: err.Error()}
	}
	if err := h.persistLocked(); err != nil {
		fmt.Fprintf(h.logw, "[tunnel %s] persist state: %v\n", msg.Egress, err)
	}
	return tunControlResp{OK: true}
}

// opClose 关闭并移除指定 egress 的隧道(docker rm 语义):关闭监听与在途连接、从 byEgress 删除、
// 并移除持久化意图。
func (h *tunnelHolder) opClose(msg tunControlMsg) tunControlResp {
	if msg.Egress == "" {
		return tunControlResp{OK: false, Error: "egress must not be empty"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	entry, ok := h.byEgress[msg.Egress]
	if !ok {
		return tunControlResp{OK: false, Error: fmt.Sprintf("no tunnel for egress %q", msg.Egress)}
	}
	h.closeEntryLocked(entry)
	delete(h.byEgress, msg.Egress)
	if err := h.persistLocked(); err != nil {
		fmt.Fprintf(h.logw, "[tunnel %s] persist state: %v\n", msg.Egress, err)
	}
	return tunControlResp{OK: true}
}

// opRestart 关闭现有监听与在途连接,再按同一 egress/listen/restart 重开,保留持久化意图。
// 条目不存在时报错。
func (h *tunnelHolder) opRestart(msg tunControlMsg) tunControlResp {
	if msg.Egress == "" {
		return tunControlResp{OK: false, Error: "egress must not be empty"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	entry, ok := h.byEgress[msg.Egress]
	if !ok {
		return tunControlResp{OK: false, Error: fmt.Sprintf("no tunnel for egress %q", msg.Egress)}
	}
	h.closeEntryLocked(entry)
	if err := h.openEntryLocked(entry.egress, entry.listen, entry.restart); err != nil {
		return tunControlResp{OK: false, Error: err.Error()}
	}
	if err := h.persistLocked(); err != nil {
		fmt.Fprintf(h.logw, "[tunnel %s] persist state: %v\n", entry.egress, err)
	}
	return tunControlResp{OK: true}
}

// opList 返回全部隧道条目(依据监听器是否存活区分 running/stopped)。
func (h *tunnelHolder) opList(msg tunControlMsg) tunControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()

	states := make([]tunnelState, 0, len(h.byEgress))
	for _, entry := range h.byEgress {
		states = append(states, entry.toState())
	}
	return tunControlResp{OK: true, Tunnels: states}
}

// serveEntry 接收来自本地监听端口的连接,并将其登记到 entry.conns 后启动双向泵。
// 监听器关闭或条目已被替换时退出。监听器在进入循环前捕获一次,避免 close 置空 listener 后
// 下一轮循环对 nil 调用 Accept 而 panic(重开时新 serveEntry 会捕获新监听器)。
func (h *tunnelHolder) serveEntry(entry *tunnelEntry) {
	ln := entry.listener
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		// 竞态防御:close/restart 可能刚好关闭了监听或替换了条目,此时丢弃已 accept 的连接。
		if entry.state != "running" || h.byEgress[entry.egress] != entry {
			h.mu.Unlock()
			_ = conn.Close()
			return
		}
		entry.conns[conn] = struct{}{}
		h.mu.Unlock()
		go h.pumpTunnelConn(entry, conn)
	}
}

// pumpTunnelConn 复用重构后的 handleTunnelConn,把 SOCKS5 握手的连接级错误路由到 entry.logf。
// 返回时从 entry.conns 移除并关闭连接。
func (h *tunnelHolder) pumpTunnelConn(entry *tunnelEntry, conn net.Conn) {
	handleTunnelConn(h.ctx, h.rb, entry.egress, conn, entry.logf)
	h.mu.Lock()
	delete(entry.conns, conn)
	h.mu.Unlock()
	_ = conn.Close()
}

// persistLocked 把当前全部条目投影为意图写入 statePath。调用方须已持有 h.mu。
func (h *tunnelHolder) persistLocked() error {
	states := make([]tunnelState, 0, len(h.byEgress))
	for _, entry := range h.byEgress {
		states = append(states, entry.toState())
	}
	return persistTunnels(h.statePath, states)
}

// loadTunnels 读取持久化的隧道意图列表;文件不存在时返回空列表(不视为错误)。
func loadTunnels(statePath string) ([]tunnelState, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var states []tunnelState
	if err := json.Unmarshal(data, &states); err != nil {
		return nil, err
	}
	return states, nil
}

// persistTunnels 原子地把隧道意图列表写回 statePath:先写 `statePath+.tmp` 再 rename(与 config 原子写一致)。
func persistTunnels(statePath string, states []tunnelState) error {
	if dir := filepath.Dir(statePath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir state dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(states, "", "  ")
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

// restore 在启动时重放持久化的隧道意图:restart=always 的重新绑定(非 loopback 触发告警),
// restart=no 的就地标记为 stopped(不绑定)。单条失败仅记录并继续。
func (h *tunnelHolder) restore() {
	states, err := loadTunnels(h.statePath)
	if err != nil {
		fmt.Fprintf(h.logw, "[Node] load tunnel state: %v\n", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, st := range states {
		if st.Egress == "" || st.Listen == "" {
			continue
		}
		if st.Restart == tunRestartAlways {
			if err := h.openEntryLocked(st.Egress, st.Listen, st.Restart); err != nil {
				fmt.Fprintf(h.logw, "[tunnel %s] open failed: %v\n", st.Egress, err)
				continue
			}
			fmt.Fprintf(h.logw, "[tunnel %s] restored (running)\n", st.Egress)
		} else {
			// restart=no:不绑定,但保留意图供 list 展示为 stopped(docker 语义:容器仍存在)。
			h.byEgress[st.Egress] = &tunnelEntry{
				egress:  st.Egress,
				listen:  st.Listen,
				restart: st.Restart,
				conns:   make(map[net.Conn]struct{}),
				state:   "stopped",
				started: st.Started,
				logf: func(format string, args ...any) {
					fmt.Fprintf(h.logw, "[tunnel "+st.Egress+"] "+format+"\n", args...)
				},
			}
			fmt.Fprintf(h.logw, "[tunnel %s] restored (stopped)\n", st.Egress)
		}
	}
}

// sendTunnelControl 经 unix socket 发送一条控制消息并解码应答。调用方负责构造 msg 与解读应答。
// conn 在返回前关闭。底层的 socket 传输错误统一返回,让上层决定是否致命。
func sendTunnelControl(sockPath string, msg tunControlMsg) (tunControlResp, error) {
	if sockPath == "" {
		sockPath = tunnelSockPath()
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return tunControlResp{}, fmt.Errorf("connect tunnel control socket %s: %w", sockPath, err)
	}
	defer conn.Close()

	// 整个控制 socket round-trip 必须有界。仅 DialTimeout 只限制了连接阶段,
	// holder 接受连接后若 handler 卡死,Encode/Decode 会让 CLI 无限阻塞。设定读写 deadline 兜底。
	timeout := 5 * time.Second
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return tunControlResp{}, fmt.Errorf("set tunnel control socket deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return tunControlResp{}, fmt.Errorf("send %s: %w", msg.Op, err)
	}
	var resp tunControlResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return tunControlResp{}, fmt.Errorf("read %s response: %w", msg.Op, err)
	}
	return resp, nil
}
