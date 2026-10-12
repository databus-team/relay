package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/relay/server"
)

// parseServerConfig 从统一 config.yaml 构建中转(Hub)配置:只有 server 段与 workspaces 有用,
// 其它字段(backend/jobs 等)被忽略。server 段缺失时用默认值(addr :8443,无 watch 目录)。
func parseServerConfig(data []byte) (server.Config, error) {
	cfg, err := config.LoadFromBytes(data)
	if err != nil {
		return server.Config{}, fmt.Errorf("parse unified config: %w", err)
	}

	sc := cfg.Server
	addr := ":8443"
	if sc != nil && sc.Addr != "" {
		addr = sc.Addr
	}

	watchDirs := make([]server.WatchDirConfig, 0, len(cfg.Workspaces))
	if sc != nil && sc.WatchRoot != "" {
		// 单根模式:目录 id 统一跟随 workspace(首个 workspace id),不再由 backend.config
		// 兼任目录身份(executor 身份已独立为 executor_id)。无 workspace 时用默认 "relay"。
		id := ""
		if len(cfg.Workspaces) > 0 {
			id = cfg.Workspaces[0].ID
		}
		if id == "" {
			id = "relay"
		}
		watchDirs = append(watchDirs, server.WatchDirConfig{ID: id, Dir: sc.WatchRoot})
	} else {
		for _, w := range cfg.Workspaces {
			dir := w.WatchDir
			if dir == "" {
				continue
			}
			if sc != nil && sc.WatchRoot != "" && !filepath.IsAbs(dir) {
				dir = filepath.Join(sc.WatchRoot, dir)
			}
			watchDirs = append(watchDirs, server.WatchDirConfig{ID: w.ID, Dir: dir, TTL: w.TTL})
		}
	}

	out := server.Config{
		Addr:      addr,
		WatchDirs: watchDirs,
		Auth:      server.AuthConfig{Type: "token"},
	}
	if sc != nil {
		out.Auth.Tokens = sc.Auth.Tokens
		out.TLS = server.TLSConfig{Enabled: sc.TLS.Enabled, CertFile: sc.TLS.CertFile, KeyFile: sc.TLS.KeyFile}
		out.TunnelEnabled = sc.TunnelEnabled
		out.MaxTunnels = sc.MaxTunnels
		out.StateDir = sc.WatchRoot
	}
	return out, nil
}

// buildServerInstance 读取统一 config.yaml 并构造 Hub 实例(relay node 的 hub 角色)。
// 接线「受控自升级」换装:自检/ACK 在 server 内完成,换装由这里注入(停旧 → .prev →
// 替换 → 原地重启)。未接线时升级通道只回执 ACK。
func buildServerInstance(path string) (*server.Server, *server.Config, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("no server config: pass -c <config>")
	}
	expanded, err := config.ExpandHome(path)
	if err != nil {
		return nil, nil, fmt.Errorf("expand config path: %w", err)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, nil, fmt.Errorf("read server config: %w", err)
	}

	cfg, err := parseServerConfig(data)
	if err != nil {
		return nil, nil, err
	}

	srv, err := server.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create server: %w", err)
	}
	srv.SetUpgradeSwap(transitSelfUpgrade)
	return srv, &cfg, nil
}

// transitSelfUpgrade 中转「受控自升级」换装闭包:入参为已通过 sha256 校验与自检、落盘好的
// 新二进制路径。步骤:备份现行二进制为 `.prev` → 原子替换 → 用新二进制原地重启
// (syscall.Exec 复用当前 argv,driver 替换进程镜像)。任一步失败保留 `.prev` 且不破坏运行中
// 实例;不做自动回滚(左移人工兜底)。
func transitSelfUpgrade(newBin string) {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("[upgrade] resolve own executable: %v", err)
		return
	}
	// 换装失败也要清理暂存二进制(不留 0755 的 /tmp 灰烬);成功后同样清理再原地重启。
	defer os.Remove(newBin)
	if err := daemon.ReplaceBinaryWithKeep(exe, newBin); err != nil {
		log.Printf("[upgrade] swap failed, .prev retained: %v", err)
		return
	}

	// 仍在以已退役的 `relay server` 命令行运行的中转,换装后要落到 node 才起得来:
	// 原地重启复用当前 argv,故这里把旧形态改写成对等节点形态。
	args := make([]string, len(os.Args))
	copy(args, os.Args)
	for i, arg := range args {
		if arg == "server" {
			args[i] = "node"
		} else if arg == "--server-config" {
			args[i] = "-c"
		}
	}

	if err := syscall.Exec(exe, args, os.Environ()); err != nil {
		log.Printf("[upgrade] in-place restart failed: %v", err)
	}
}
