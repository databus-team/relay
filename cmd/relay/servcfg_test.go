package main

import (
	"reflect"
	"testing"
	"time"
)

// 统一 config.yaml 单根模式:watch_root 设了 → 注册一个 watch 覆盖整个根。
func TestParseServerConfigSingleRoot(t *testing.T) {
	data := []byte(`
server:
  addr: ":9443"
  watch_root: /data/relay
  auth:
    tokens: ["tok-a"]
  tls:
    enabled: true
    cert_file: /x/cert.pem
    key_file: /x/key.pem

backend:
  config:
    executor_id: storage

workspaces:
  - id: rel
    watch_dir: app/patches
    ttl: 10m
`)
	cfg, err := parseServerConfig(data)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if cfg.Addr != ":9443" {
		t.Errorf("addr: %q", cfg.Addr)
	}
	if !reflect.DeepEqual(cfg.Auth.Tokens, []string{"tok-a"}) {
		t.Errorf("tokens: %v", cfg.Auth.Tokens)
	}
	if cfg.Auth.Type != "token" {
		t.Errorf("auth type: %q", cfg.Auth.Type)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != "/x/cert.pem" {
		t.Errorf("tls: %+v", cfg.TLS)
	}
	// 台账持久化目录取 watch_root,否则重启后离线记录丢失。
	if cfg.StateDir != "/data/relay" {
		t.Errorf("state dir: %q", cfg.StateDir)
	}
	if len(cfg.WatchDirs) != 1 {
		t.Fatalf("want 1 root watch, got %d", len(cfg.WatchDirs))
	}
	got := cfg.WatchDirs[0]
	// 目录 id 统一跟随 workspace:首个 workspace id=rel(backend.executor_id 不再兼任目录)。
	if got.ID != "rel" || got.Dir != "/data/relay" {
		t.Errorf("root watch: id=%q dir=%q; want id=rel dir=/data/relay", got.ID, got.Dir)
	}
}

// 未设 watch_root → 每个 watch 映射到各自服务器目录(兼容)。
func TestParseServerConfigPerWatch(t *testing.T) {
	data := []byte(`
server:
  addr: ":9443"
  auth:
    tokens: ["tok-a"]

workspaces:
  - id: rel
    watch_dir: /data/relay/app/patches
    ttl: 10m
  - id: abs
    watch_dir: /opt/abs
`)
	cfg, err := parseServerConfig(data)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	want := []struct{ id, dir string }{
		{"rel", "/data/relay/app/patches"},
		{"abs", "/opt/abs"},
	}
	if len(cfg.WatchDirs) != len(want) {
		t.Fatalf("watchDirs: %d", len(cfg.WatchDirs))
	}
	for i, w := range want {
		got := cfg.WatchDirs[i]
		if got.ID != w.id || got.Dir != w.dir {
			t.Errorf("watch[%d]: id=%q dir=%q; want id=%q dir=%q", i, got.ID, got.Dir, w.id, w.dir)
		}
	}
	if cfg.WatchDirs[0].TTL != 10*time.Minute {
		t.Errorf("ttl: %v", cfg.WatchDirs[0].TTL)
	}
}

// max_tunnels / tunnel_enabled 透传;未配置时为 0,交给 server.New 退到 defaultMaxTunnels。
func TestParseServerConfigMaxTunnels(t *testing.T) {
	enabled := []byte(`
server:
  addr: ":9443"
  tunnel_enabled: true
  max_tunnels: 32
workspaces:
  - id: rel
    watch_dir: /data/relay
`)
	cfg, err := parseServerConfig(enabled)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if !cfg.TunnelEnabled {
		t.Errorf("tunnel_enabled should be true")
	}
	if cfg.MaxTunnels != 32 {
		t.Errorf("maxTunnels: %d, want 32", cfg.MaxTunnels)
	}

	def := []byte("workspaces:\n  - id: only\n    watch_dir: just/this\n")
	dcfg, err := parseServerConfig(def)
	if err != nil {
		t.Fatalf("parseServerConfig(default): %v", err)
	}
	if dcfg.MaxTunnels != 0 {
		t.Errorf("default maxTunnels: %d, want 0", dcfg.MaxTunnels)
	}
	if dcfg.TunnelEnabled {
		t.Errorf("tunnel_enabled should default to false")
	}
}

// 未提供 server 段时 addr 默认 :8443,workspace 照常映射。
func TestParseServerConfigDefaults(t *testing.T) {
	data := []byte(`
workspaces:
  - id: only
    watch_dir: just/this
`)
	cfg, err := parseServerConfig(data)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if cfg.Addr != ":8443" {
		t.Errorf("default addr: %q", cfg.Addr)
	}
	if len(cfg.WatchDirs) != 1 || cfg.WatchDirs[0].Dir != "just/this" {
		t.Errorf("watchDirs: %+v", cfg.WatchDirs)
	}
}

// 单根模式的存储 id 优先级:首个 workspace id > "relay"。
func TestParseServerConfigSingleRootWatchID(t *testing.T) {
	data := []byte(`
server:
  addr: ":8443"
  watch_root: /home/devpod/storage
  auth:
    tokens: ["tok-a"]
`)
	cfg, err := parseServerConfig(data)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if len(cfg.WatchDirs) != 1 {
		t.Fatalf("want 1 root watch, got %d", len(cfg.WatchDirs))
	}
	// 无 backend.config.watch_id、无 workspace → 回退到 "relay"。
	if cfg.WatchDirs[0].ID != "relay" || cfg.WatchDirs[0].Dir != "/home/devpod/storage" {
		t.Errorf("root watch: %+v", cfg.WatchDirs[0])
	}
	if !reflect.DeepEqual(cfg.Auth.Tokens, []string{"tok-a"}) {
		t.Errorf("tokens: %v", cfg.Auth.Tokens)
	}
}

// 单根模式下 backend.executor_id 不再兼任目录 id:存储 id 只跟随 workspace,无 workspace 时
// 回退 "relay"(executor 身份与目录分层解耦)。
func TestParseServerConfigSingleRootBackendExecutorIDIgnored(t *testing.T) {
	data := []byte(`
server:
  addr: ":8443"
  watch_root: /home/devpod/storage
backend:
  type: relay
  config:
    executor_id: storage
`)
	cfg, err := parseServerConfig(data)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if len(cfg.WatchDirs) != 1 || cfg.WatchDirs[0].ID != "relay" {
		t.Errorf("root watch: %+v", cfg.WatchDirs)
	}
}

// 配置路径由调用方(-c)唯一决定:空路径不再回退到别的旗标,直接报错。
func TestBuildServerInstanceRequiresPath(t *testing.T) {
	if _, _, err := buildServerInstance(""); err == nil {
		t.Fatal("expected an error for an empty config path")
	}
}
