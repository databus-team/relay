package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/relay/protocol"
)

// TunnelInstanceInfo 描述一个本地 SOCKS5 隧道实例的综合状态。隧道已并入 node 进程,
// 没有独立进程与独立日志,故 PID/LOG 两列给出的是宿主 node 的值(字段名据此取 node_pid)。
type TunnelInstanceInfo struct {
	Status      string `json:"status"` // "running" | "stopped"
	NodePID     int    `json:"node_pid"`
	Listen      string `json:"listen"`
	Egress      string `json:"egress"`
	EgressReady bool   `json:"egress_ready"`
	EgressText  string `json:"egress_status"`
	LogFile     string `json:"log_file"`
}

// shortenPath 把家目录替换为 ~ 缩短显示
func shortenPath(path string) string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		if strings.HasPrefix(path, u.HomeDir) {
			return "~" + strings.TrimPrefix(path, u.HomeDir)
		}
	}
	return path
}

// queryClusterStatusSafe 尝试向中转探测全网节点在线状态，快速超时 (1.5s) 且错误静默降级
func queryClusterStatusSafe() map[string]protocol.StatusSegment {
	cfg, err := config.Load(*configPath)
	if err != nil || cfg.Backend.Type != "relay" {
		return nil
	}

	rb, err := getRelayBackend(cfg)
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	st, err := rb.StatusWithUsage(ctx, false)
	if err != nil || !st.OK {
		return nil
	}

	nodesMap := make(map[string]protocol.StatusSegment, len(st.Executors)+1)
	// Transit hub 自身
	nodesMap["hub"] = st.Seg1
	for _, e := range st.Executors {
		nodesMap[e.ExecutorID] = e.Total
	}
	return nodesMap
}

// runTunnelStatus 展示隧道实例状态（支持表格与 --json 输出）。事实来源是本地 node 的
// 隧道持有者 socket（list 操作），而非扫描 pid 文件/日志——隧道已并入 node 进程，PID 与日志
// 都归属该 node。node 未运行时不去隐式拉起，直接报告无隧道。
func runTunnelStatus() error {
	// 先校验配置可读：全网探测（queryClusterStatusSafe）对配置错误是静默降级的，
	// 这里显式暴露，避免用户把「配置坏了」误读成「所有出口都下线」。
	if _, err := config.Load(*configPath); err != nil {
		return err
	}

	targetNode := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)

	nodeStatus := daemon.Get(daemon.PidFile("node"))
	if nodeStatus.Err != nil {
		return nodeStatus.Err
	}
	if !nodeStatus.Running {
		if *tunnelJSON {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("node not running; no tunnel instances.")
		return nil
	}

	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpList})
	if err != nil {
		return fmt.Errorf("query tunnels: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("list tunnels: %s", resp.Error)
	}

	// 指定了目标节点时按 egress 过滤（保留 stopped 条目，与 docker ps -a 语义一致）。
	states := filterTunnels(resp.Tunnels, targetNode, true)

	// 有隧道时才做全网探测(1.5s 超时、断网降级为 nil);空列表直接进空态,不做无谓的 relay 往返。
	var clusterStatus map[string]protocol.StatusSegment
	if len(states) > 0 {
		clusterStatus = queryClusterStatusSafe()
	}

	rows := make([]TunnelInstanceInfo, 0, len(states))
	for _, st := range states {
		egressReady := false
		egressStatusText := "unknown"

		if clusterStatus != nil {
			if seg, exists := clusterStatus[st.Egress]; exists {
				if seg.Unavailable != "" {
					egressStatusText = "Offline"
				} else if seg.LatencyMS != nil {
					egressReady = true
					egressStatusText = fmt.Sprintf("Ready (%d ms)", *seg.LatencyMS)
				} else {
					egressReady = true
					egressStatusText = "Ready"
				}
			} else {
				egressStatusText = "Offline"
			}
		}

		rows = append(rows, TunnelInstanceInfo{
			Status:      st.State,
			NodePID:     nodeStatus.Pid,
			Listen:      st.Listen,
			Egress:      st.Egress,
			EgressReady: egressReady,
			EgressText:  egressStatusText,
			LogFile:     daemon.LogFile("node"),
		})
	}

	// 1. JSON 格式输出
	if *tunnelJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	// 2. 空态:node 在跑但没有任何隧道意图时,给出明确提示而非只打表头。
	if len(rows) == 0 {
		if targetNode != "" {
			fmt.Printf("No tunnel found for egress %q.\n", targetNode)
		} else {
			fmt.Println("No tunnel instances found.")
		}
		return nil
	}

	// 3. 对齐表格格式输出
	fmt.Printf("%-10s %-8s %-18s %-16s %-18s %s\n",
		"STATUS", "NODE PID", "LISTEN", "EGRESS", "EGRESS STATUS", "LOG")

	for _, r := range rows {
		pidStr := "-"
		if r.NodePID > 0 {
			pidStr = fmt.Sprintf("%d", r.NodePID)
		}
		logDisplay := shortenPath(r.LogFile)
		fmt.Printf("%-10s %-8s %-18s %-16s %-18s %s\n",
			r.Status, pidStr, r.Listen, r.Egress, r.EgressText, logDisplay)
	}

	return nil
}

// tunnelLogLineMatches 判断一行 node 日志是否属于指定 egress 的隧道。隧道日志统一带字面前缀
// `[tunnel <egress>]`;此处要求连右括号一起匹配,避免 egress 前缀相同的隧道互相误吞
// (如 `eg-node` 不应匹配 `[tunnel eg-node-2]`)。纯函数,便于单测。
func tunnelLogLineMatches(line, egress string) bool {
	return strings.Contains(line, "[tunnel "+egress+"]")
}

// readTunnelLogTail 有界读取 node 日志中属于 egress 的最后至多 tailLines 行:从文件末尾按窗口
// 回退读取,命中不足时窗口翻倍(上限为文件大小)。node.log 无轮转、无上限增长,故避免整文件扫描。
func readTunnelLogTail(logPath, egress string, tailLines int) ([]string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}

	window := int64(64 * 1024)
	if w := int64(tailLines) * 512; w > window {
		window = w
	}
	for {
		if window > size {
			window = size
		}
		start := size - window
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, err
		}
		text := string(buf)
		if start > 0 {
			// 窗口首行可能是被截断的半行,丢弃到第一个换行之后。
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			} else {
				text = ""
			}
		}
		var matched []string
		for _, line := range strings.Split(text, "\n") {
			if line != "" && tunnelLogLineMatches(line, egress) {
				matched = append(matched, line)
			}
		}
		if len(matched) >= tailLines || start == 0 {
			if len(matched) > tailLines {
				matched = matched[len(matched)-tailLines:]
			}
			return matched, nil
		}
		window *= 2
	}
}

// tunnelEgressKnown 询问本地 node 的隧道持有者是否认识该 egress。隧道是否存在以 holder 为准,
// 而不是日志内容:新开且健康的隧道不会写任何 `[tunnel <egress>]` 前缀行,「零匹配行」推不出
// 「不是隧道」。node 未运行(无持有者)时一律返回 false。
func tunnelEgressKnown(egress string) bool {
	if egress == "" || !daemon.Get(daemon.PidFile("node")).Running {
		return false
	}
	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpList})
	if err != nil || !resp.OK {
		return false
	}
	for _, st := range resp.Tunnels {
		if st.Egress == egress {
			return true
		}
	}
	return false
}

// showTunnelLogs 把 `relay logs <egress>` 落到 node 日志上,只呈现该隧道的行。调用方须先经
// tunnelEgressKnown 确认 holder 认识该 egress:此时零匹配行是「尚无输出」而非「不是隧道」,
// 照常打印已有行(可能一行都没有)并在 follow 时进入跟读。node 前台运行时隧道日志走 stderr、
// 不落 node.log,故仅当日志文件缺失才补一条提示。
func showTunnelLogs(egress string, follow bool, tailLines int) {
	if tailLines <= 0 {
		tailLines = 100
	}
	logPath := daemon.LogFile("node")
	lines, err := readTunnelLogTail(logPath, egress, tailLines)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "node log %s not found (the node may be running in the foreground; its tunnel logs go to stderr).\n", shortenPath(logPath))
		}
		return
	}
	w := bufio.NewWriter(os.Stdout)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	_ = w.Flush()

	if follow {
		followTunnelLog(logPath, egress)
	}
}

// followTunnelLog 轮询 node 日志新增内容,只打印属于 egress 的行,直到收到 SIGINT/SIGTERM。
// 打开一次并持有 fd,每 tick 仅 Stat + 续读;仅在出错或日志被截断时重新定位。
func followTunnelLog(logPath, egress string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "follow tunnel log: %v\n", err)
		return
	}
	defer f.Close()

	// 从当前文件末尾起追,避免重复打印刚刚 tail 过的内容。
	var offset int64
	if fi, err := f.Stat(); err == nil {
		offset = fi.Size()
	}
	pending := "" // 跨轮次的半行缓冲

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := f.Stat()
			if err != nil {
				return
			}
			if fi.Size() < offset { // rotate/truncate:从头再读
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return
				}
				offset, pending = 0, ""
			}
			if fi.Size() == offset {
				continue
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return
			}
			var chunk []byte
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					chunk = append(chunk, buf[:n]...)
					offset += int64(n)
				}
				if rerr != nil {
					break
				}
			}
			pending += string(chunk)
			for {
				idx := strings.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := pending[:idx]
				pending = pending[idx+1:]
				if tunnelLogLineMatches(line, egress) {
					fmt.Println(line)
				}
			}
		}
	}
}
