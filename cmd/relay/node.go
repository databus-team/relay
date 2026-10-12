package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/config"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/server"
	"github.com/user/relay/internal/watcher"
)

var (
	nodeCmd = kingpin.Command("node", "Run unified peer node daemon (hub and agent roles)")

	// 子命令定义
	_               = nodeCmd.Command("run", "Run node daemon in foreground")
	_               = nodeCmd.Command("start", "Start node daemon in background")
	_               = nodeCmd.Command("stop", "Stop running node daemon")
	_               = nodeCmd.Command("status", "Show status of node daemon")
	_               = nodeCmd.Command("restart", "Restart node daemon")
	nodeUpgradeCmd  = nodeCmd.Command("upgrade", "Upgrade node daemon binary")
	nodeUpgradePath = nodeUpgradeCmd.Arg("upgrade-path", "Path to new relay binary (with action=upgrade)").String()
	nodeConfigPath  = nodeCmd.Flag("server-config", "Path to server config file (YAML, for backward compatibility)").String()
)

// runNode 运行统一对等节点 (Node)
// 根据配置文件的 node.roles 声明：
// - 若包含 hub: 初始化并启动 Server (Hub)
// - 若包含 agent: 启动 Watcher (Agent)
// - 若同时包含 hub 与 agent: 通过 srv.Ready() 闩同步，等待 Hub 端口成功监听后再拨号 Agent
func runNode() error {
	path := *configPath
	if nodeConfigPath != nil && *nodeConfigPath != "" {
		path = *nodeConfigPath
	}
	cfgPath, err := config.ExpandHome(path)
	if err != nil {
		return fmt.Errorf("expand config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 校验配置合法性与角色
	isHub := cfg.IsHub()
	isAgent := cfg.IsAgent()

	if !isHub && !isAgent {
		return fmt.Errorf("node must have at least one role: hub and/or agent")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer relaybackend.Cleanup()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nNode shutting down...")
		cancel()
	}()

	var (
		srv     *server.Server
		srvConf *server.Config
		wg      sync.WaitGroup
	)

	// 1. 如果包含 Hub 角色，创建并启动 Hub
	if isHub {
		var err error
		srv, srvConf, err = buildServerInstance(cfgPath)
		if err != nil {
			return fmt.Errorf("build hub instance: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("[Node] Starting Hub on %s (node_id: %s)\n", srvConf.Addr, cfg.Node.ID)
			for _, wd := range srvConf.WatchDirs {
				fmt.Printf("  Hub Watching: %s -> %s\n", wd.ID, wd.Dir)
			}
			if srvConf.TLS.Enabled {
				fmt.Printf("  Hub TLS: %s\n", srvConf.TLS.CertFile)
			}
			if err := srv.Serve(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "[Node] Hub error: %v\n", err)
				cancel()
			}
		}()
	}

	// 2. 如果包含 Agent 角色，等待 Hub 就绪（若同机启动）后启动 Agent
	if isAgent {
		if isHub && srv != nil {
			select {
			case <-srv.Ready():
				// Hub 已经监听就绪
			case <-ctx.Done():
				return nil
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("[Node] Starting Agent (node_id: %s)\n", cfg.Node.ID)
			relaybackend.SetExecutorRole(true)

			// 确保作为 Agent 启动时开启执行器角色
			cfg.NormalizeNode()

			w, err := watcher.New(cfg, cfgPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[Node] Failed to create watcher: %v\n", err)
				cancel()
				return
			}
			w.SetPushJobHandler(runLocalJobsForPush)

			// 端口转发 + SOCKS5 隧道两个常驻持有者(本机 node 作为 dockerd,接收 CLI 控制以维持
			// 本地监听,即使发起 CLI 已退出)。二者共用同一个 relay backend:客户端池按
			// url|token|executor_id 复用,本就是同一底层连接;由本 goroutine 独占其生命周期,
			// 避免任一 holder 早退(如控制 socket 绑定失败)连带拆掉另一个的传输与 executor 注册。
			go func() {
				rb, err := getRelayBackend(cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[Node] Holders disabled: %v\n", err)
					return
				}
				defer rb.Close()

				// 端口转发持有者:`relay run -d -p` 的本地转发。
				go func() {
					holder := newForwardHolder(ctx, rb, forwardSockPath())
					if err := holder.Run(); err != nil && err != context.Canceled {
						fmt.Fprintf(os.Stderr, "[Node] Port-forward holder error: %v\n", err)
					}
				}()

				// 隧道持有者:各条 SOCKS5 出网隧道;意图持久化到 ~/.relay/tunnels.json,node 重启时
				// 按 restart 策略回放。
				holder := newTunnelHolder(ctx, rb, tunnelSockPath())
				if err := holder.Run(); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "[Node] Tunnel holder error: %v\n", err)
				}
			}()

			if err := w.Run(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "[Node] Agent error: %v\n", err)
				cancel()
			}
		}()
	}

	wg.Wait()
	return nil
}
