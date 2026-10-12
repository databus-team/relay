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

// restartPolicyValue 把一对重启策略旗标(策略字符串 + --no-restart 布尔)归一为标准策略值
// (always|no)。纯函数,不读全局旗标:relay run 的任务与隧道两种模式共用同一份校验,
// --no-restart 优先,缺省与空值都归一为 no。
func restartPolicyValue(policy string, noRestart bool) (string, error) {
	if noRestart {
		return tunRestartNo, nil
	}
	switch p := strings.ToLower(strings.TrimSpace(policy)); p {
	case "", tunRestartNo:
		return tunRestartNo, nil
	case tunRestartAlways:
		return tunRestartAlways, nil
	default:
		return "", fmt.Errorf("invalid --restart %q (want always|no)", p)
	}
}

// startTunnel 是隧道创建的共享动作:确保本机 node(holder)在线,再下发 open 控制消息。
// 唯一入口是 `relay run -t tunnel`(KD6),控制消息代码不外分叉。
func startTunnel(cfg *config.Config, target, listen, policy string, warnw io.Writer) error {
	if cfg.Backend.Type != "relay" {
		return fmt.Errorf("tunnel requires backend.type=relay, got %q", cfg.Backend.Type)
	}
	if err := ensureLocalRelayNode(cfg, tunnelSockPath()); err != nil {
		return err
	}
	return openTunnelViaHolder(tunnelSockPath(), target, listen, policy, warnw)
}

// openTunnelViaHolder 经本地 node 的控制 unix socket 下发 open 并处理应答/告警。
// warnw 接收非 loopback 与持久化告警(生产即调用方 stderr;测试可注入缓冲以断言文案)。
func openTunnelViaHolder(sockPath, target, listen, policy string, warnw io.Writer) error {
	resp, err := sendTunnelControl(sockPath, tunControlMsg{
		Op:      tunOpOpen,
		Egress:  target,
		Listen:  listen,
		Restart: policy,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		// 最常见的失败是 holder 绑不上监听地址——多半是残留的旧 `tunnel-<egress>` 独立 daemon
		// 还占着同一端口。此处就地清掉它(与 stop 的收尾同一动作,见 stopLegacyTunnelDaemon):
		// create 刚失败,该 egress 既没有任务也没有持久化意图,提示里的 `relay stop <egress>`
		// 解析不出对象、走不到 stopLocalTunnel,是条死路。清掉后如实报「已停掉,可重试」;
		// 只有清不掉时才回落到原提示(仍保留 kill 兜底)。
		if stopLegacyTunnelDaemon(target) {
			return fmt.Errorf("start tunnel %s: %s; a legacy tunnel daemon %s was holding the listen address and has been stopped — retry the command", target, resp.Error, tunnelInstanceName(target))
		}
		return fmt.Errorf("start tunnel %s: %s%s", target, resp.Error, tunnelLegacyDaemonHint(target))
	}
	// 非 loopback 绑定 = 一个无认证、网络上任意主机可用的 SOCKS5 入口。holder 侧也会记一条到
	// node.log,但那是 node 的 stderr(daemon 已重定向),发起者本人看不到,故 CLI 同样打到 stderr。
	if !isLoopbackAddr(listen) {
		fmt.Fprint(warnw, tunnelNonLoopbackWarning(listen))
	}
	if resp.Warning != "" {
		fmt.Fprintf(warnw, "Warning: %s\n", resp.Warning)
	}
	fmt.Printf("tunnel %s started: SOCKS5 on %s -> egress %s (restart=%s)\n", target, listen, target, policy)
	return nil
}

// tunnelNonLoopbackWarning 返回非 loopback 监听的告警文本。holder(node.log)与 CLI(stderr)
// 共用同一措辞,避免两处措辞漂移。
func tunnelNonLoopbackWarning(listen string) string {
	return fmt.Sprintf("WARNING: listening on %q (non-loopback). This tunnel has no auth; any host that can reach this port can use it to reach the executor's network.\n", listen)
}

// tunnelLegacyDaemonHint 探测是否仍有旧的每出口独立 `tunnel-<egress>` daemon 在跑(已废弃的部署
// 形态)。若在跑,它很可能占着同一监听地址导致 holder 绑定失败;返回一段清理提示,否则返回空串。
func tunnelLegacyDaemonHint(egress string) string {
	inst := tunnelInstanceName(egress)
	st := daemon.Get(daemon.PidFile(inst))
	if !st.Running {
		return ""
	}
	return fmt.Sprintf("; a legacy tunnel daemon %s is still running (pid %d) and may hold the listen address — remove it with `relay stop %s` or `kill %d`", inst, st.Pid, egress, st.Pid)
}

// stopLegacyTunnelDaemon 尽力停掉某 egress 对应的旧式独立 `tunnel-<egress>` daemon(已废弃的每出口
// daemon 形态),返回是否确有进程被停。它是隧道 stop 的收尾与「创建失败时排除占位者」共用的同一份
// 清理:egress 是执行方身份,可能含非法文件名字符,实例名由 tunnelInstanceName 净化。
func stopLegacyTunnelDaemon(egress string) bool {
	inst := tunnelInstanceName(egress)
	if !daemon.Get(daemon.PidFile(inst)).Running {
		return false
	}
	daemonStop(inst)
	return true
}

// tunnelStopOutcome 是一次隧道停止请求的结果。区分「不认识这个名字」与「确实停掉了」,因为
// R1(名字不存在要报错)与 R2(已停止的对象再停一次不算失败)对这两种情形要求不同的答复。
type tunnelStopOutcome int

const (
	// tunnelStopUnknown:node 在运行但 holder 拒绝(没有该 egress / 传输失败)—— 名字不在。
	// 拒绝/传输的底层原因不再被吞掉,随 err 一起交回调用方(见 stopLocalTunnel),便于区分报告。
	tunnelStopUnknown tunnelStopOutcome = iota
	// tunnelStopClosed:经 holder 关闭成功(关闭监听、在途连接并移除意图)。
	tunnelStopClosed
	// tunnelStopRemoved:node 未运行,仅从意图文件移除(R18)。
	tunnelStopRemoved
	// tunnelStopAbsent:node 未运行且意图文件里也没有 —— 已忘掉的对象。
	tunnelStopAbsent
)

// stopSucceeded 报告一次隧道 stop 是否真的把对象停/忘掉了(R2 的幂等语义:Closed 与 Removed
// 都算成功;Unknown/Absent 是「名字不在」,由调用方决定是否报错)。
func stopSucceeded(out tunnelStopOutcome, err error) bool {
	return err == nil && (out == tunnelStopClosed || out == tunnelStopRemoved)
}

// stopLocalTunnel 是本机隧道「停」的唯一实现(R18):node 在运行时经 holder 下发 close(关闭监听与
// 在途连接、移除意图);node 未运行时降级为直接移除意图文件里的条目(否则 `--restart=always` 会在
// 下次 node 启动时被 restore 重放,与「关闭并遗忘」(docker rm)语义相悖)。两条路径最后都清理可能
// 残留的旧式独立 tunnel daemon。停不掉时(tunnelStopUnknown)第二个返回值带上底层原因——传输失败
// 与 holder 拒绝都如实透出,不再塌缩成「名字不在」;成功/已停止/已忘掉三类结果的文案保持不变。
// statePath/sockPath/nodeRunning 由调用方注入,便于在不碰真实
// ~/.relay 的前提下覆盖 node 未运行这一分支(见 lifecycle_test.go)。
func stopLocalTunnel(statePath, sockPath, egress string, nodeRunning bool) (tunnelStopOutcome, error) {
	if egress == "" {
		return tunnelStopUnknown, nil
	}
	// 旧部署形态的独立隧道 daemon(已废弃):若还在跑,一并清理,避免它继续占用监听地址
	// (与旧每出口 daemon 时代的 stop 收尾一致)。egress 是执行方身份,可能含非法文件名字符,
	// 实例名由 tunnelInstanceName 净化。
	defer stopLegacyTunnelDaemon(egress)

	if !nodeRunning {
		// node 未运行:没有 holder 可下发 close,但持久化意图仍须移除。
		removed, rerr := removePersistedTunnel(statePath, egress)
		switch {
		case rerr != nil:
			fmt.Fprintf(os.Stderr, "Warning: remove persisted tunnel %s: %v\n", egress, rerr)
			return tunnelStopUnknown, rerr
		case removed:
			fmt.Printf("node not running; removed persisted tunnel %s\n", egress)
			return tunnelStopRemoved, nil
		default:
			fmt.Printf("node not running; no tunnel for %s to stop\n", egress)
			return tunnelStopAbsent, nil
		}
	}
	resp, err := sendTunnelControl(sockPath, tunControlMsg{Op: tunOpClose, Egress: egress})
	switch {
	case err != nil:
		// 传输层失败(控制 socket 不可达等):Warning 照打,但底层原因也随 err 交回调用方 ——
		// 否则「停不掉」会与「名字不在」混为一谈(调用方只会报 no task or tunnel named X)。
		fmt.Fprintf(os.Stderr, "Warning: stop tunnel %s: %v\n", egress, err)
		return tunnelStopUnknown, err
	case !resp.OK:
		// holder 明确拒绝(如没有该 egress):同样保留它的原话,不静默降级。
		fmt.Fprintf(os.Stderr, "Warning: stop tunnel %s: %s\n", egress, resp.Error)
		return tunnelStopUnknown, errors.New(resp.Error)
	default:
		if resp.Warning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", resp.Warning)
		}
		fmt.Printf("tunnel %s stopped\n", egress)
		return tunnelStopClosed, nil
	}
}

// removePersistedTunnel 从隧道意图文件里移除指定 egress 的条目(holder 不可达时的清理路径,
// 如 node 未运行)。返回是否确有条目被移除;文件不存在或本就没有该条目时返回 (false, nil)。
// 读写复用 holder 的 loadTunnels/persistTunnels,保持 tmp+rename 的原子写。
func removePersistedTunnel(statePath, egress string) (bool, error) {
	states, err := loadTunnels(statePath)
	if err != nil {
		return false, err
	}
	kept := make([]tunnelState, 0, len(states))
	removed := false
	for _, st := range states {
		if st.Egress == egress {
			removed = true
			continue
		}
		kept = append(kept, st)
	}
	if !removed {
		return false, nil
	}
	if err := persistTunnels(statePath, kept); err != nil {
		return false, err
	}
	return true, nil
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
