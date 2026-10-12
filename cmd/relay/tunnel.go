package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	relaybackend "github.com/user/relay/internal/relay/backend"
)

const (
	socksVer5       = 0x05
	socksCmdConnect = 0x01
	socksRepSuccess = 0x00
	socksRepFailure = 0x01
	socksRepReject  = 0x07 // command not supported

	// handshakeTimeout 本地 SOCKS5 握手上限:只连不发/半开连接在限定时间内结束。
	handshakeTimeout = 10 * time.Second
)

// tunnelRestartPolicyValue 把 --restart/--no-restart 解析为标准策略值(always|no)。
// --no-restart 是 --restart=no 的快捷方式(且优先);缺省与空值都归一为 no。
func tunnelRestartPolicyValue() (string, error) {
	if *tunnelNoRestart {
		return tunRestartNo, nil
	}
	switch policy := strings.ToLower(strings.TrimSpace(*tunnelRestartPolicy)); policy {
	case "", tunRestartNo:
		return tunRestartNo, nil
	case tunRestartAlways:
		return tunRestartAlways, nil
	default:
		return "", fmt.Errorf("invalid --restart %q (want always|no)", policy)
	}
}

// runTunnelStart 启动一条由本地 node 持有的 SOCKS5 隧道:先确保 node(agent 角色)在线,
// 再经 holder unix socket 下发 open 消息(携带 egress/listen/restart 策略)。只有 start 会
// 自动拉起 node;stop/restart 只控制既有隧道,不隐式启动常驻进程。
func runTunnelStart() error {
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Backend.Type != "relay" {
		return fmt.Errorf("tunnel requires backend.type=relay, got %q", cfg.Backend.Type)
	}

	// 出口 node 选择:`-w` 是「执行方注册的节点身份 executor_id」(见 `relay status` 的
	// `executors[].executor_id`),不是本地工作区名。
	target, err := tunnelRequireExecutor()
	if err != nil {
		return err
	}

	policy, err := tunnelRestartPolicyValue()
	if err != nil {
		return err
	}

	if err := ensureLocalRelayNode(cfg, tunnelSockPath()); err != nil {
		return err
	}

	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{
		Op:      tunOpOpen,
		Egress:  target,
		Listen:  *tunnelListen,
		Restart: policy,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		// 最常见的失败是 holder 绑不上监听地址——多半是残留的旧 `tunnel-<egress>` 独立 daemon
		// 还占着同一端口,附上可操作的清理提示。
		return fmt.Errorf("start tunnel %s: %s%s", target, resp.Error, tunnelLegacyDaemonHint(target))
	}
	fmt.Printf("tunnel %s started: SOCKS5 on %s -> egress %s (restart=%s)\n", target, *tunnelListen, target, policy)
	return nil
}

// tunnelLegacyDaemonHint 探测是否仍有旧的每出口独立 `tunnel-<egress>` daemon 在跑(已废弃的部署
// 形态)。若在跑,它很可能占着同一监听地址导致 holder 绑定失败;返回一段清理提示,否则返回空串。
func tunnelLegacyDaemonHint(egress string) string {
	inst := tunnelInstanceName(egress)
	st := daemon.Get(daemon.PidFile(inst))
	if !st.Running {
		return ""
	}
	return fmt.Sprintf("; a legacy tunnel daemon %s is still running (pid %d) and may hold the listen address — remove it with `relay tunnel stop -w %s` or `kill %d`", inst, st.Pid, egress, st.Pid)
}

// runTunnelStop 停止一条由本地 node 持有的隧道。node 未运行时视为「已经没有该隧道可停」,
// 幂等成功;最后总是顺带清理可能残留的旧独立 tunnel daemon。
func runTunnelStop() {
	target, err := tunnelRequireExecutor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	if !daemon.Get(daemon.PidFile("node")).Running {
		fmt.Printf("node not running; no tunnel for %s to stop\n", target)
	} else {
		resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpClose, Egress: target})
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		case !resp.OK:
			fmt.Fprintf(os.Stderr, "Warning: stop tunnel %s: %s\n", target, resp.Error)
		default:
			fmt.Printf("tunnel %s stopped\n", target)
		}
	}

	// 旧部署形态的独立隧道 daemon(已废弃):若还在跑,一并清理,避免它继续占用监听地址。
	if legacy := daemon.Get(daemon.PidFile(tunnelInstanceName(target))); legacy.Running {
		daemonStop(tunnelInstanceName(target))
	}
}

// runTunnelRestart 重启一条 node 持有的隧道(按原 egress/listen/restart 策略重开,无需重敲旗标)。
// node 未运行时直接报错而不自动拉起:restart 只对既有隧道有意义。
func runTunnelRestart() error {
	target, err := tunnelRequireExecutor()
	if err != nil {
		return err
	}
	if !daemon.Get(daemon.PidFile("node")).Running {
		return fmt.Errorf("local node is not running; nothing to restart for %s (start it with `relay node start`, then `relay tunnel start -w %s`)", target, target)
	}

	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpRestart, Egress: target})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("restart tunnel %s: %s", target, resp.Error)
	}
	fmt.Printf("tunnel %s restarted\n", target)
	return nil
}

// handleTunnelConn 处理一条本地 SOCKS5 连接:SOCKS5 握手解 → TunnelOpen → 建连成功后
// 双向字节泵(本地 SOCKS ↔ 远端 executor 的真实连接)。任一侧失败时仅向 SOCKS 回失败帧,不建内网流量。
func handleTunnelConn(ctx context.Context, rb *relaybackend.RelayBackend, watchID string, soc net.Conn, logf func(format string, args ...any)) {
	defer soc.Close()

	// 握手要有界:只连不发/半开的连接在限定时间内结束,不长期占用 fd/goroutine。
	br := bufio.NewReader(soc)
	_ = soc.SetDeadline(time.Now().Add(handshakeTimeout))
	host, port, br, err := socks5Handshake(soc, br)
	_ = soc.SetDeadline(time.Time{})
	if err != nil {
		if !errors.Is(err, io.EOF) {
			socks5Reply(soc, socksRepFailure)
		}
		return
	}

	stream, err := rb.TunnelOpen(ctx, watchID, host, port)
	if err != nil {
		logf("%s:%d egress %s: %v", host, port, watchID, err)
		socks5Reply(soc, socksRepFailure)
		return
	}

	socks5Reply(soc, socksRepSuccess)

	// 握手包与应用层首包间歇:当目标是 SSH 等「服务端连通即抢先推流 Banner」的协议时,
	// 远端首帧已在 TunnelOpen 窗口被预存就绪。如果紧接着立刻写入 soc,本地 loopback TCP 协议栈
	// 会将 10 字节的 SOCKS5 REP 与随后的 SSH Banner 粘在同一数据段发送。本地某些透明代理/TUN 工具
	// 在 SOCKS 状态机尚未切入透传时读到混合包,会把前置握手控制字符透传给 OpenSSH,触发
	// "kex_exchange_identification: banner line contains invalid characters" / invalid format。
	// 此处让步 15ms,确保 SOCKS5 握手应答被客户端单独消费并完成状态机切换。
	time.Sleep(15 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(2)
	// 本地 SOCKS → 远端:从与握手共享的 bufio.Reader 读,确保客户端随 CONNECT 管道化的首段
	// 应用字节不被握手的内部缓冲吞噬;读到字节即经隧道发往目标;读完/出错→#5 端隧道。
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := br.Read(buf)
			if n > 0 {
				// TunnelStream.Send 是异步入队(writeLoop 在另一 goroutine marshal/写出),
				// 若把读到的 buf[:n] 引用直接传入,下一轮 br.Read 复用同一 buffer 会在写队列
				// 消化前覆盖它——大流量(如 SSH)会被静默破坏。故每块拷贝一份再投递。
				piece := make([]byte, n)
				copy(piece, buf[:n])
				if serr := stream.Send(piece); serr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		stream.Close()
	}()
	// 远端 → 本地 SOCKS:读到远端字节写回 SOCKS;隧道关闭则关闭对端。
	go func() {
		defer wg.Done()
		for {
			d, rerr := stream.Recv()
			if rerr != nil {
				break
			}
			if _, werr := soc.Write(d); werr != nil {
				break
			}
		}
		_ = soc.Close()
	}()
	wg.Wait()
}

// socks5Handshake 完成 RFC1928「无认证」握手并解析 CONNECT 目标,返回 (host, port, br)。
// 传入复用同一 bufio.Reader(握手后由调用方继续从 br 读取应用字节,避免管道化数据被封存在
// 握手的内部缓冲里被丢弃);回复中写入仍经原始 conn。
func socks5Handshake(conn net.Conn, br *bufio.Reader) (string, uint16, *bufio.Reader, error) {
	// 版本 + 认证方式协商
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return "", 0, br, err
	}
	if hdr[0] != socksVer5 {
		return "", 0, br, fmt.Errorf("bad SOCKS version %d", hdr[0])
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return "", 0, br, err
	}
	if _, err := conn.Write([]byte{socksVer5, 0x00}); err != nil { // 无认证
		return "", 0, br, err
	}

	// CONNECT 请求
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return "", 0, br, err
	}
	if req[0] != socksVer5 {
		return "", 0, br, fmt.Errorf("bad request version %d", req[0])
	}
	if req[1] != socksCmdConnect {
		return "", 0, br, errors.New("only CONNECT supported")
	}

	var host string
	switch req[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(br, ip); err != nil {
			return "", 0, br, err
		}
		host = fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err := io.ReadFull(br, ip); err != nil {
			return "", 0, br, err
		}
		host = net.IP(ip).String()
	case 0x03: // 域名
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return "", 0, br, err
		}
		domain := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, domain); err != nil {
			return "", 0, br, err
		}
		host = string(domain)
	default:
		return "", 0, br, fmt.Errorf("unsupported address type %d", req[3])
	}

	portb := make([]byte, 2)
	if _, err := io.ReadFull(br, portb); err != nil {
		return "", 0, br, err
	}
	return host, binary.BigEndian.Uint16(portb), br, nil
}

// socks5Reply 回写 SOCKS5 连接结果帧(REP)。
func socks5Reply(conn net.Conn, rep byte) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	// VER REP RSV ATYP BND.ADDR BND.PORT(全部 0)
	_, _ = conn.Write([]byte{socksVer5, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}

// isLoopbackAddr 判断 listen 地址是否回环(仅本机);空 host 视为本机。
func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || strings.EqualFold(host, "localhost") || strings.EqualFold(host, "::1") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.Equal(net.IPv4(127, 0, 0, 1)))
}
