package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/relay/server"
	"gopkg.in/yaml.v3"
)

var (
	serverCmd = kingpin.Command("server", "Run relay server")

	serverConfigPath = serverCmd.Flag("server-config", "Path to server config file (YAML)").String()
	serverAddr       = serverCmd.Flag("addr", "Server listen address").String()
	serverWatchDirs  = serverCmd.Flag("watch", "Watch directory (format: id:path)").Strings()
	serverToken      = serverCmd.Flag("token", "Authentication token").Strings()
	serverTLSCert    = serverCmd.Flag("tls-cert", "TLS certificate file").String()
	serverTLSKey     = serverCmd.Flag("tls-key", "TLS key file").String()
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
}

func runServer() error {
	fileCfg := serverYAMLConfig{}
	if *serverConfigPath != "" {
		expanded := *serverConfigPath
		if strings.HasPrefix(expanded, "~") {
			home, err := os.UserHomeDir()
			if err == nil {
				expanded = home + expanded[1:]
			}
		}
		data, err := os.ReadFile(expanded)
		if err != nil {
			return fmt.Errorf("read server config: %w", err)
		}
		data = []byte(os.ExpandEnv(string(data)))
		if err := yaml.Unmarshal(data, &fileCfg); err != nil {
			return fmt.Errorf("parse server config: %w", err)
		}
	}

	addr := fileCfg.Addr
	if *serverAddr != "" {
		addr = *serverAddr
	}
	if addr == "" {
		addr = ":8443"
	}

	watchDirs := make([]server.WatchDirConfig, 0)
	for _, w := range fileCfg.Watch {
		watchDirs = append(watchDirs, server.WatchDirConfig{ID: w.ID, Dir: w.Dir, TTL: w.TTL})
	}
	for _, wd := range *serverWatchDirs {
		parts := strings.SplitN(wd, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			fmt.Fprintf(os.Stderr, "Invalid watch format: %s (expected id:path)\n", wd)
			continue
		}
		watchDirs = append(watchDirs, server.WatchDirConfig{ID: parts[0], Dir: config.NormalizeWindowsPath(parts[1])})
	}

	tokens := fileCfg.Auth.Tokens
	if len(*serverToken) > 0 {
		tokens = *serverToken
	}

	tlsCert := fileCfg.TLS.CertFile
	tlsKey := fileCfg.TLS.KeyFile
	tlsEnabled := fileCfg.TLS.Enabled
	if *serverTLSCert != "" {
		tlsCert = *serverTLSCert
		tlsEnabled = true
	}
	if *serverTLSKey != "" {
		tlsKey = *serverTLSKey
	}

	cfg := server.Config{
		Addr:      addr,
		WatchDirs: watchDirs,
		Auth:      server.AuthConfig{Type: "token", Tokens: tokens},
		TLS:       server.TLSConfig{Enabled: tlsEnabled, CertFile: tlsCert, KeyFile: tlsKey},
	}

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
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

	fmt.Printf("Starting relay server on %s\n", addr)
	for _, wd := range watchDirs {
		fmt.Printf("  Watching: %s -> %s\n", wd.ID, wd.Dir)
	}
	if tlsEnabled {
		fmt.Printf("  TLS: %s\n", tlsCert)
	}

	return srv.Serve(ctx)
}
