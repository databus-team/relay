package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/protocol"
)

// forwardSockPath 返回本地 node 端口转发控制 socket 的绝对路径 `~/.relay/forward.sock`。
// 复用 daemon.SockFile 与 node.pid/node.log 共用同一 baseDir,避免路径分叉。
func forwardSockPath() string { return daemon.SockFile("forward") }

// 控制 socket 的操作码常量:避免 register/unregister 以裸字符串在多处出现。
const (
	opRegister   = "register"
	opUnregister = "unregister"
)

// fwdControlMsg 是一条来自 CLI 的控制消息(JSON 单行,如 docker CLI -> dockerd unix socket)。
// register: 为 task_id 注册一组端口转发;unregister: 撤销该 task 全部转发。
type fwdControlMsg struct {
	Op       string            `json:"op"`
	TaskID   string            `json:"task_id,omitempty"`
	Executor string            `json:"executor,omitempty"`
	Specs    []PortForwardSpec `json:"specs,omitempty"`
}

// fwdControlResp 是对控制消息的应答(JSON 单行)。
// Registered/Rejected 是 register 时逐条规格的结果:Rejected 为端口冲突等未能绑定(视为告警而非致命)。
type fwdControlResp struct {
	OK         bool     `json:"ok"`
	Error      string   `json:"error,omitempty"`
	Registered []string `json:"registered,omitempty"`
	Rejected   []string `json:"rejected,omitempty"`
}

// fwdForwarder 是一个已注册到 task_id 下的活跃端口转发:监听本地端口,每条连接经中转隧道转发到远端目标。
// addr 为注册时的 "bind:port",供 byAddr 冲突检测与 unregister 精确删除复用。
type fwdForwarder struct {
	taskID   string
	executor string
	addr     string // canonical "bind:port"
	spec     PortForwardSpec
	listener net.Listener
}

// forwardHolder 是常驻于本地 node(agent 角色)进程内的端口转发持有者,类比 dockerd。
// 它通过 unix socket 接受 CLI 的 register/unregister 控制,并负责维持
// 各转发的本地监听;即使发起 `relay run -d` 的 CLI 已退出,转发依然存活。
type forwardHolder struct {
	rb       *relaybackend.RelayBackend
	ctx      context.Context
	sockPath string

	mu     sync.Mutex
	byAddr map[string]*fwdForwarder // "bind:port" -> 转发者,用于端口占用冲突检测 + 同任务复用判定(KTD5)
	byTask map[string][]*fwdForwarder
}

// newForwardHolder 构造一个端口转发持有者。sockPath 为空时使用默认 `~/.relay/forward.sock`。
func newForwardHolder(ctx context.Context, rb *relaybackend.RelayBackend, sockPath string) *forwardHolder {
	if sockPath == "" {
		sockPath = forwardSockPath()
	}
	return &forwardHolder{
		rb:       rb,
		ctx:      ctx,
		sockPath: sockPath,
		byAddr:   make(map[string]*fwdForwarder),
		byTask:   make(map[string][]*fwdForwarder),
	}
}

// Run 监听 unix socket 并处理控制消息,直到构造时的 ctx 取消或 Accept 出错。返回时 socket 已清理。
func (h *forwardHolder) Run() error {
	_ = os.Remove(h.sockPath)
	if dir := filepath.Dir(h.sockPath); dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	ln, err := net.Listen("unix", h.sockPath)
	if err != nil {
		return fmt.Errorf("listen forward control socket %s: %w", h.sockPath, err)
	}
	defer os.Remove(h.sockPath)

	fmt.Printf("[Node] Port-forward holder listening on unix socket %s\n", h.sockPath)

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
	default:
		return fwdControlResp{OK: false, Error: fmt.Sprintf("unknown op %q", msg.Op)}
	}
}

// opRegister 注册一组端口转发。逐条处理:某规格端口已被占用(或绑定失败)时仅拒绝该条并记录,
// 不影响其它规格,也不覆盖已有转发。非 loopback 绑定发出与前台转发一致的告警。
// 幂等(KTD5):同一 task id 命中同一地址视为复用而不是冲突 —— `relay stop` 之后再 `relay start`,
// 或本机 node 重启后重登记,规则本就该在;真正的跨任务占用仍按冲突拒绝。
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
			fmt.Fprintf(os.Stderr, "WARNING: listening on %s without authentication\n", addr)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			resp.Rejected = append(resp.Rejected, fmt.Sprintf("listen %s failed: %v", addr, err))
			continue
		}
		fw := &fwdForwarder{taskID: msg.TaskID, executor: msg.Executor, addr: addr, spec: spec, listener: ln}
		h.byAddr[addr] = fw
		h.byTask[msg.TaskID] = append(h.byTask[msg.TaskID], fw)
		resp.Registered = append(resp.Registered, addr)
		go h.serveForwarder(fw) // accept 循环不触碰 h.mu,可在锁内安全启动
	}
	return resp
}

// opUnregister 关闭并移除指定 task_id 下的全部转发。
func (h *forwardHolder) opUnregister(msg fwdControlMsg) fwdControlResp {
	h.mu.Lock()
	defer h.mu.Unlock()

	resp := fwdControlResp{OK: true}
	for _, fw := range h.byTask[msg.TaskID] {
		delete(h.byAddr, fw.addr)
		_ = fw.listener.Close()
	}
	delete(h.byTask, msg.TaskID)
	return resp
}

// serveForwarder 接收来自本地监听端口的连接,并逐条建立中转隧道。
func (h *forwardHolder) serveForwarder(fw *fwdForwarder) {
	for {
		conn, err := fw.listener.Accept()
		if err != nil {
			return
		}
		go h.pumpFwdConn(fw, conn)
	}
}

// pumpFwdConn 复用 handlePortForwardConn 的双向泵:本地 TCP -> TunnelStream -> 远端目标。
func (h *forwardHolder) pumpFwdConn(fw *fwdForwarder, conn net.Conn) {
	handlePortForwardConn(h.ctx, h.rb, fw.executor, &fw.spec, conn)
}

// ensureLocalRelayNode 确保本地 node daemon 处于运行状态(未运行则启动),
// 并等待其控制 socket(sockPath:端口转发 holder 的 forward.sock,或隧道 holder 的 tunnel.sock)
// 就绪——以成功拨号为准,而非仅存在文件。CLI 侧 `relay run -d -p` 与 `relay run -t tunnel` 前调用。
// holder 只在 node 的 agent 角色块内启动,因此:
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
