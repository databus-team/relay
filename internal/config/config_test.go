package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `
name: file-exchange
version: 1

backend:
  type: local
  config:
    base_dir: /tmp
    command_dir: /commands

workspaces:
  - id: test
    watch_dir: /tmp
    paths: ["*.txt"]
    local_dir: /tmp
    jobs:
      - id: test
        type: exec
        cmd: echo {file_name}
        cwd: /tmp

interval_seconds: 60
`

	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Name != "file-exchange" {
		t.Errorf("Expected name 'file-exchange', got '%s'", cfg.Name)
	}
	if cfg.Version != 1 {
		t.Errorf("Expected version 1, got %d", cfg.Version)
	}
	if cfg.Backend.Type != "local" {
		t.Errorf("Expected backend type 'local', got '%s'", cfg.Backend.Type)
	}
	if len(cfg.Workspaces) != 1 {
		t.Errorf("Expected 1 watcher, got %d", len(cfg.Workspaces))
	}
	if cfg.Workspaces[0].ID != "test" {
		t.Errorf("Expected watcher id 'test', got '%s'", cfg.Workspaces[0].ID)
	}
}

// 统一 config.yaml:含 server 段 + watch.ttl;server 段只是被解析出来,不破坏既有字段。
func TestLoadUnifiedConfigWithServer(t *testing.T) {
	data := []byte(`
name: relay
version: 2

server:
  addr: ":9443"
  watch_root: /data/relay
  auth:
    tokens: ["tok-a", "tok-b"]
  tls:
    enabled: true
    cert_file: /x/cert.pem
    key_file: /x/key.pem

backend:
  type: relay
  config:
    url: "wss://t:8443/relay"
    watch_id: patches

workspaces:
  - id: patches
    watch_dir: app/patches
    local_dir: /srv/repo
    paths: ["*.patch"]
    ttl: 30m
    jobs:
      - id: apply
        type: exec
        cmd: git am {file_path}
`)
	cfg, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}

	if cfg.Server == nil {
		t.Fatalf("expected server section to be parsed")
	}
	if cfg.Server.Addr != ":9443" {
		t.Errorf("server.addr: %q", cfg.Server.Addr)
	}
	if cfg.Server.WatchRoot != "/data/relay" {
		t.Errorf("server.watch_root: %q", cfg.Server.WatchRoot)
	}
	if len(cfg.Server.Auth.Tokens) != 2 || cfg.Server.Auth.Tokens[0] != "tok-a" {
		t.Errorf("server.auth.tokens: %v", cfg.Server.Auth.Tokens)
	}
	if !cfg.Server.TLS.Enabled || cfg.Server.TLS.CertFile != "/x/cert.pem" {
		t.Errorf("server.tls: %+v", cfg.Server.TLS)
	}

	if len(cfg.Workspaces) != 1 {
		t.Fatalf("watches: %d", len(cfg.Workspaces))
	}
	if cfg.Workspaces[0].TTL != 30*time.Minute {
		t.Errorf("watch.ttl: %v (want 30m)", cfg.Workspaces[0].TTL)
	}
	if len(cfg.Workspaces[0].Jobs) != 1 {
		t.Errorf("jobs: %d", len(cfg.Workspaces[0].Jobs))
	}
}
