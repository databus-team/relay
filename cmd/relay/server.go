package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/relay/server"
	"gopkg.in/yaml.v3"
)

var (
	serverCmd = kingpin.Command("server", "Transit server daemon (deprecated, use node instead)")

	// 子命令定义
	_               = serverCmd.Command("run", "Run server in foreground")
	_               = serverCmd.Command("start", "Start server daemon in background")
	_               = serverCmd.Command("stop", "Stop running server daemon")
	_               = serverCmd.Command("status", "Show status of server daemon")
	_               = serverCmd.Command("restart", "Restart server daemon")
	serverUpgradeCmd = serverCmd.Command("upgrade", "Upgrade server daemon binary")
	serverUpgradePath = serverUpgradeCmd.Arg("upgrade-path", "Path to new relay binary (with action=upgrade)").String()

	serverConfigPath = serverCmd.Flag("server-config", "Path to server config file (YAML)").String()
	serverAddr       = serverCmd.Flag("addr", "Server listen address").String()
	serverWatchDirs  = serverCmd.Flag("watch", "Watch directory (format: id:path)").Strings()
	serverToken      = serverCmd.Flag("token", "Authentication token").Strings()
	serverTLSCert    = serverCmd.Flag("tls-cert", "TLS certificate file").String()
	serverTLSKey     = serverCmd.Flag("tls-key", "TLS key file").String()
	serverMaxTunnels = serverCmd.Flag("max-tunnels", "Max concurrent tunnels (<=0 uses default 256)").Int()
)

type serverYAMLConfig struct {
	Addr  string `yaml:"addr"`
	Watch []struct {
		ID  string        `yaml:"id"`
		Dir string        `yaml:"dir"`
		TTL time.Duration `yaml:"ttl"`
	} `yaml:"watch"`
	Auth struct {
		Tokens []string `yaml:"tokens"`
	} `yaml:"auth"`
	TLS struct {
		Enabled  bool   `yaml:"enabled"`
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
	TunnelEnabled bool `yaml:"tunnel_enabled"`
	MaxTunnels    int  `yaml:"max_tunnels"`
}

// serverBaseConfig 统一保存映射后的中转基础配置(文件来源),再叠加 CLI overrides。
type serverBaseConfig struct {
	addr          string
	watchDirs     []server.WatchDirConfig
	tokens        []string
	tls           server.TLSConfig
	tunnelEnabled bool
	maxTunnels    int
	stateDir      string // 已知 executor 台账的持久化目录(取 server.watch_root;空=不持久化)
}

// hasServerSection 判断文件是否含顶层 `server:` 段(统一 config.yaml)。
func hasServerSection(data []byte) bool {
	var probe struct {
		Server *struct{} `yaml:"server"`
	}
	yaml.Unmarshal(data, &probe)
	return probe.Server != nil
}

// unifiedServerBase 从统一 config.yaml(含 server 段)构建中转配置。
// 只有 server 段 + watch 列表有用;其它字段(backend/jobs 等)被忽略。
func unifiedServerBase(data []byte) (serverBaseConfig, error) {
	cfg, err := config.LoadFromBytes(data)
	if err != nil {
		return serverBaseConfig{}, fmt.Errorf("parse unified config: %w", err)
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

	tokens := []string(nil)
	tls := server.TLSConfig{}
	tunnelEnabled := false
	maxTunnels := 0
	stateDir := ""
	if sc != nil {
		tokens = sc.Auth.Tokens
		tls = server.TLSConfig{Enabled: sc.TLS.Enabled, CertFile: sc.TLS.CertFile, KeyFile: sc.TLS.KeyFile}
		tunnelEnabled = sc.TunnelEnabled
		maxTunnels = sc.MaxTunnels
		stateDir = sc.WatchRoot
	}

	return serverBaseConfig{addr: addr, watchDirs: watchDirs, tokens: tokens, tls: tls, tunnelEnabled: tunnelEnabled, maxTunnels: maxTunnels, stateDir: stateDir}, nil
}

// legacyServerBase 从旧式 server.yaml(顶层 addr/watch/auth/tls)构建 server 配置。
func legacyServerBase(data []byte) (serverBaseConfig, error) {
	var fileCfg serverYAMLConfig
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		return serverBaseConfig{}, fmt.Errorf("parse server config: %w", err)
	}

	addr := fileCfg.Addr
	if addr == "" {
		addr = ":8443"
	}
	watchDirs := make([]server.WatchDirConfig, 0, len(fileCfg.Watch))
	for _, w := range fileCfg.Watch {
		watchDirs = append(watchDirs, server.WatchDirConfig{ID: w.ID, Dir: w.Dir, TTL: w.TTL})
	}
	return serverBaseConfig{
		addr:          addr,
		watchDirs:     watchDirs,
		tokens:        fileCfg.Auth.Tokens,
		tls:           server.TLSConfig{Enabled: fileCfg.TLS.Enabled, CertFile: fileCfg.TLS.CertFile, KeyFile: fileCfg.TLS.KeyFile},
		tunnelEnabled: fileCfg.TunnelEnabled,
		maxTunnels:    fileCfg.MaxTunnels,
	}, nil
}

func buildServerInstance(path string) (*server.Server, *server.Config, error) {
	if path == "" {
		path = *serverConfigPath
	}
	if path == "" && nodeConfigPath != nil && *nodeConfigPath != "" {
		path = *nodeConfigPath
	}
	if path == "" {
		// 未指定 --server-config 时,回退到全局 -c/--config 客户端配置文件(统一 config.yaml)。
		path = *configPath
	}
	if path == "" {
		return nil, nil, fmt.Errorf("no server config: pass --server-config or -c")
	}

	expanded := path
	if strings.HasPrefix(expanded, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			expanded = home + expanded[1:]
		}
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, nil, fmt.Errorf("read server config: %w", err)
	}
	data = []byte(os.ExpandEnv(string(data)))

	var base serverBaseConfig
	if hasServerSection(data) {
		base, err = unifiedServerBase(data)
	} else {
		base, err = legacyServerBase(data)
	}
	if err != nil {
		return nil, nil, err
	}

	addr := base.addr
	if serverAddr != nil && *serverAddr != "" {
		addr = *serverAddr
	}

	watchDirs := base.watchDirs
	if serverWatchDirs != nil {
		for _, wd := range *serverWatchDirs {
			parts := strings.SplitN(wd, ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				fmt.Fprintf(os.Stderr, "Invalid watch format: %s (expected id:path)\n", wd)
				continue
			}
			watchDirs = append(watchDirs, server.WatchDirConfig{ID: parts[0], Dir: config.NormalizeWindowsPath(parts[1])})
		}
	}

	tokens := base.tokens
	if serverToken != nil && len(*serverToken) > 0 {
		tokens = *serverToken
	}

	tlsCert := base.tls.CertFile
	tlsKey := base.tls.KeyFile
	tlsEnabled := base.tls.Enabled
	if serverTLSCert != nil && *serverTLSCert != "" {
		tlsCert = *serverTLSCert
		tlsEnabled = true
	}
	if serverTLSKey != nil && *serverTLSKey != "" {
		tlsKey = *serverTLSKey
	}

	maxTunnels := base.maxTunnels
	if serverMaxTunnels != nil && *serverMaxTunnels > 0 {
		maxTunnels = *serverMaxTunnels
	}

	cfg := server.Config{
		Addr:          addr,
		WatchDirs:     watchDirs,
		Auth:          server.AuthConfig{Type: "token", Tokens: tokens},
		TLS:           server.TLSConfig{Enabled: tlsEnabled, CertFile: tlsCert, KeyFile: tlsKey},
		TunnelEnabled: base.tunnelEnabled,
		MaxTunnels:    maxTunnels,
		StateDir:      base.stateDir,
	}

	srv, err := server.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create server: %w", err)
	}
	// 接线「受控自升级」换装:自检/ACK 在 server 内完成,换装由这里注入(停旧 → .prev →
	// 替换 → 原地重启)。未接线时升级通道只回执 ACK。
	srv.SetUpgradeSwap(transitSelfUpgrade)

	return srv, &cfg, nil
}

func runServer() error {
	srv, cfg, err := buildServerInstance("")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	fmt.Printf("Starting relay server on %s\n", cfg.Addr)
	for _, wd := range cfg.WatchDirs {
		fmt.Printf("  Watching: %s -> %s\n", wd.ID, wd.Dir)
	}
	if cfg.TLS.Enabled {
		fmt.Printf("  TLS: %s\n", cfg.TLS.CertFile)
	}

	return srv.Serve(ctx)
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

	// 平滑切为对等节点 daemon: 将启动参数中的 server 切换为 node
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
