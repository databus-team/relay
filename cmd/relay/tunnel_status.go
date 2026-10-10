package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/relay/backend"
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

var tunnelHeaderRe = regexp.MustCompile(`tunnel:\s+SOCKS5\s+on\s+([^\s]+)\s+->\s+egress\s+([^\s]+)`)

// parseTunnelMeta 从实例名、日志文件及运行环境提取 listen 地址与 egress 节点
func parseTunnelMeta(instName, logFile string, defaultListen string) (listen, egress string) {
	// 1. egress 节点优先从实例名获取（形如 tunnel-<egress>）
	egress = instName
	if strings.HasPrefix(instName, "tunnel-") {
		egress = strings.TrimPrefix(instName, "tunnel-")
	}

	listen = defaultListen
	if listen == "" {
		listen = "127.0.0.1:1080"
	}

	// 2. 尝试从 logFile 的前若干行提取真实启动参数
	if logFile != "" {
		if f, err := os.Open(logFile); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			count := 0
			for scanner.Scan() && count < 20 {
				line := scanner.Text()
				if m := tunnelHeaderRe.FindStringSubmatch(line); len(m) == 3 {
					listen = m[1]
					if m[2] != "" {
						egress = m[2]
					}
					break
				}
				count++
			}
		}
	}

	return listen, egress
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

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		return nil
	}

	rb, ok := b.(*backend.RelayBackend)
	if !ok {
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

// daemonStatusTunnel 展示隧道实例状态（支持表格与 --json 输出）
func daemonStatusTunnel() error {
	insts, err := daemon.ListInstances("tunnel")
	if err != nil {
		return err
	}

	targetNode := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)

	// 若指定了目标节点，过滤对应实例
	if targetNode != "" {
		var filtered []daemon.Instance
		for _, in := range insts {
			if in.Name == targetNode || in.Name == tunnelInstanceName(targetNode) || strings.TrimPrefix(in.Name, "tunnel-") == targetNode {
				filtered = append(filtered, in)
			}
		}
		insts = filtered
	}

	if len(insts) == 0 {
		if *tunnelJSON {
			fmt.Println("[]")
			return nil
		}
		if targetNode != "" {
			fmt.Printf("No tunnel instance found for node %q.\n", targetNode)
		} else {
			fmt.Println("No active tunnel instances found.")
		}
		return nil
	}

	// 并发/快速探测中转节点在线台账 (超时 1.5s，断网则为 nil)
	clusterStatus := queryClusterStatusSafe()

	var rows []TunnelInstanceInfo
	for _, in := range insts {
		st := "stopped"
		pid := 0
		if in.Status.Running {
			st = "running"
			pid = in.Status.Pid
		}

		cleanName := strings.TrimPrefix(in.Name, "tunnel-")
		listenAddr, egressNode := parseTunnelMeta(in.Name, in.LogFile, *tunnelListen)

		egressReady := false
		egressStatusText := "unknown"

		if clusterStatus != nil {
			if seg, exists := clusterStatus[egressNode]; exists {
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
			Name:        cleanName,
			Status:      st,
			PID:         pid,
			Listen:      listenAddr,
			Egress:      egressNode,
			EgressReady: egressReady,
			EgressText:  egressStatusText,
			LogFile:     in.LogFile,
		})
	}

	// 1. JSON 格式输出
	if *tunnelJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	// 2. 对齐表格格式输出
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
