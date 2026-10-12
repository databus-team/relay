package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/relay/protocol"
)

// TunnelInstanceInfo 描述一个本地 SOCKS5 隧道实例的综合状态
type TunnelInstanceInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // "running" | "stopped"
	PID         int    `json:"pid"`
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

// daemonStatusTunnel 展示隧道实例状态（支持表格与 --json 输出）。事实来源是本地 node 的
// 隧道持有者 socket（list 操作），而非扫描 pid 文件/日志——隧道已并入 node 进程，PID 与日志
// 都归属该 node。node 未运行时不去隐式拉起，直接报告无隧道。
func daemonStatusTunnel() error {
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

	states := resp.Tunnels
	// 指定了目标节点时按 egress 过滤（保留 stopped 条目，与 docker ps -a 语义一致）。
	if targetNode != "" {
		filtered := make([]tunnelState, 0, len(states))
		for _, st := range states {
			if st.Egress == targetNode {
				filtered = append(filtered, st)
			}
		}
		states = filtered
	}

	// 并发/快速探测中转节点在线台账 (超时 1.5s，断网则为 nil)
	clusterStatus := queryClusterStatusSafe()

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
			Name:        st.Egress,
			Status:      st.State,
			PID:         nodeStatus.Pid,
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
	fmt.Printf("%-16s %-10s %-8s %-18s %-16s %-18s %s\n",
		"NAME", "STATUS", "PID", "LISTEN", "EGRESS", "EGRESS STATUS", "LOG")

	for _, r := range rows {
		pidStr := "-"
		if r.PID > 0 {
			pidStr = fmt.Sprintf("%d", r.PID)
		}
		logDisplay := shortenPath(r.LogFile)
		fmt.Printf("%-16s %-10s %-8s %-18s %-16s %-18s %s\n",
			r.Name, r.Status, pidStr, r.Listen, r.Egress, r.EgressText, logDisplay)
	}

	return nil
}
