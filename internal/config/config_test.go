package config

import (
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
    executor_id: patches

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

// 表驱动测试:config-sync 保留执行方身份字段(MergeConfigPreservingIdentity)。
func TestMergeConfigPreservingIdentity(t *testing.T) {
	baseCfg := `name: relay
version: 2
backend:
  type: relay
  config:
    url: ws://transit:8443/relay
    token: tok
    executor_id: site-b
    executor: true
    executor_dir: /remote/proj
    network_allow:
      - "10.0.0.0/8@80,443"
      - "api.internal.com@443"
    headers:
      Cookie: "sid=abc"
`
	incomingTrimmed := `name: relay
version: 3
backend:
  type: relay
  config:
    url: ws://newhost:8443/relay
    token: newtok
workspaces:
  - id: web-app-patches
    watch_dir: .
    paths: ["*.patch"]
`

	t.Run("identity survives + shared overlaid", func(t *testing.T) {
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(baseCfg), 0644); err != nil {
			t.Fatal(err)
		}
		merged, err := MergeConfigPreservingIdentity([]byte(incomingTrimmed), p)
		if err != nil {
			t.Fatal(err)
		}
		m := unmarshalMap(t, merged)
		got := m["backend"].(map[string]interface{})["config"].(map[string]interface{})
		if got["executor_id"] != "site-b" {
			t.Errorf("watch_id = %v (want preserved site-b)", got["executor_id"])
		}
		if got["executor"] != true {
			t.Errorf("executor = %v (want preserved true)", got["executor"])
		}
		if got["executor_dir"] != "/remote/proj" {
			t.Errorf("executor_dir = %v", got["executor_dir"])
		}
		// 共享字段取 incoming
		if got["url"] != "ws://newhost:8443/relay" {
			t.Errorf("url = %v (want incoming)", got["url"])
		}
		if got["token"] != "newtok" {
			t.Errorf("token = %v (want incoming)", got["token"])
		}
		// 切片/映射完整往返
		na := got["network_allow"].([]interface{})
		if len(na) != 2 || na[0] != "10.0.0.0/8@80,443" {
			t.Errorf("network_allow = %v", got["network_allow"])
		}
		hd := got["headers"].(map[string]interface{})
		if hd["Cookie"] != "sid=abc" {
			t.Errorf("headers = %v", got["headers"])
		}
	})

	t.Run("base wins when both present", func(t *testing.T) {
		stdin := `backend:
  config:
    executor: false
    executor_id: shadow
`
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(baseCfg), 0644); err != nil {
			t.Fatal(err)
		}
		merged, err := MergeConfigPreservingIdentity([]byte(stdin), p)
		if err != nil {
			t.Fatal(err)
		}
		got := unmarshalMap(t, merged)["backend"].(map[string]interface{})["config"].(map[string]interface{})
		if got["executor"] != true {
			t.Errorf("executor = %v (want base true)", got["executor"])
		}
		if got["executor_id"] != "site-b" {
			t.Errorf("watch_id = %v (want base site-b)", got["executor_id"])
		}
	})

	t.Run("no base file degrades to incoming", func(t *testing.T) {
		dir := t.TempDir()
		merged, err := MergeConfigPreservingIdentity([]byte(incomingTrimmed), dir+"/missing.yaml")
		if err != nil {
			t.Fatal(err)
		}
		m := unmarshalMap(t, merged)["backend"].(map[string]interface{})["config"].(map[string]interface{})
		if _, ok := m["executor_id"]; ok {
			t.Errorf("watch_id should be absent (identity not invented)")
		}
		if m["url"] != "ws://newhost:8443/relay" {
			t.Errorf("url = %v", m["url"])
		}
	})

	t.Run("incoming without backend.config still keeps identity", func(t *testing.T) {
		incomingNoBackend := `name: relay
version: 3
workspaces:
  - id: w
`
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(baseCfg), 0644); err != nil {
			t.Fatal(err)
		}
		merged, err := MergeConfigPreservingIdentity([]byte(incomingNoBackend), p)
		if err != nil {
			t.Fatal(err)
		}
		m := unmarshalMap(t, merged)
		cfg := m["backend"].(map[string]interface{})["config"].(map[string]interface{})
		if cfg["executor"] != true || cfg["executor_id"] != "site-b" {
			t.Errorf("identity not preserved: %v", cfg)
		}
		if len(m["workspaces"].([]interface{})) != 1 {
			t.Errorf("workspaces not overlaid")
		}
	})

	t.Run("corrupt base → falls back to incoming", func(t *testing.T) {
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(":::not yaml"), 0644); err != nil {
			t.Fatal(err)
		}
		merged, err := MergeConfigPreservingIdentity([]byte(incomingTrimmed), p)
		if err != nil {
			t.Fatal(err)
		}
		m := unmarshalMap(t, merged)["backend"].(map[string]interface{})["config"].(map[string]interface{})
		if _, ok := m["executor"]; ok {
			t.Errorf("executor should be absent from corrupt-base fallback")
		}
	})

	t.Run("bad incoming → error", func(t *testing.T) {
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(baseCfg), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := MergeConfigPreservingIdentity([]byte("not: [valid"), p); err == nil {
			t.Errorf("want parse error for invalid incoming")
		}
	})
}

func unmarshalMap(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal merged: %v\n%s", err, b)
	}
	return m
}
