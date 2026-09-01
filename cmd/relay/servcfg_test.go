package main

import (
	"reflect"
	"testing"
	"time"
)

// 统一 config.yaml 单根模式:watch_root 设了 → 注册一个 watch 覆盖整个根。
func TestUnifiedServerBaseSingleRoot(t *testing.T) {
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
    watch_id: storage

watch:
  - id: rel
    watch_dir: app/patches
    ttl: 10m
`)
	base, err := unifiedServerBase(data)
	if err != nil {
		t.Fatalf("unifiedServerBase: %v", err)
	}
	if base.addr != ":9443" {
		t.Errorf("addr: %q", base.addr)
	}
	if !reflect.DeepEqual(base.tokens, []string{"tok-a"}) {
		t.Errorf("tokens: %v", base.tokens)
	}
	if !base.tls.Enabled || base.tls.CertFile != "/x/cert.pem" {
		t.Errorf("tls: %+v", base.tls)
	}
	if len(base.watchDirs) != 1 {
		t.Fatalf("want 1 root watch, got %d", len(base.watchDirs))
	}
	got := base.watchDirs[0]
	if got.ID != "storage" || got.Dir != "/data/relay" {
		t.Errorf("root watch: id=%q dir=%q; want id=storage dir=/data/relay", got.ID, got.Dir)
	}
}

// 未设 watch_root → 每个 watch 映射到各自服务器目录(兼容)。
func TestUnifiedServerBasePerWatch(t *testing.T) {
	data := []byte(`
server:
  addr: ":9443"
  auth:
    tokens: ["tok-a"]

watch:
  - id: rel
    watch_dir: /data/relay/app/patches
    ttl: 10m
  - id: abs
    watch_dir: /opt/abs
`)
	base, err := unifiedServerBase(data)
	if err != nil {
		t.Fatalf("unifiedServerBase: %v", err)
	}
	want := []struct{ id, dir string }{
		{"rel", "/data/relay/app/patches"},
		{"abs", "/opt/abs"},
	}
	if len(base.watchDirs) != len(want) {
		t.Fatalf("watchDirs: %d", len(base.watchDirs))
	}
	for i, w := range want {
		got := base.watchDirs[i]
		if got.ID != w.id || got.Dir != w.dir {
			t.Errorf("watch[%d]: id=%q dir=%q; want id=%q dir=%q", i, got.ID, got.Dir, w.id, w.dir)
		}
	}
	if base.watchDirs[0].TTL != 10*time.Minute {
		t.Errorf("ttl: %v", base.watchDirs[0].TTL)
	}
}

// 无 server 段的旧式 server.yaml → legacy serverBaseConfig。
func TestLegacyServerBase(t *testing.T) {
	data := []byte(`
addr: ":9000"
watch:
  - id: old
    dir: /var/relay/old
auth:
  tokens: ["tok-o"]
tls:
  enabled: false
`)
	base, err := legacyServerBase(data)
	if err != nil {
		t.Fatalf("legacyServerBase: %v", err)
	}
	if base.addr != ":9000" {
		t.Errorf("addr: %q", base.addr)
	}
	if len(base.watchDirs) != 1 || base.watchDirs[0].ID != "old" || base.watchDirs[0].Dir != "/var/relay/old" {
		t.Errorf("watchDirs: %+v", base.watchDirs)
	}
	if !reflect.DeepEqual(base.tokens, []string{"tok-o"}) {
		t.Errorf("tokens: %v", base.tokens)
	}
}

// admin 无 server 段时 hasServerSection 返回 false,有则 true。
func TestHasServerSection(t *testing.T) {
	if !hasServerSection([]byte("server:\n  addr: ':1'\n")) {
		t.Errorf("expected server section detected")
	}
	if hasServerSection([]byte("backend:\n  type: local\n")) {
		t.Errorf("expected no server section")
	}
}

// 未提供 server 段时 addr 默认 :8443,watch_root 为空则不拼接。
func TestUnifiedServerBaseDefaults(t *testing.T) {
	data := []byte(`
watch:
  - id: only
    watch_dir: just/this
`)
	base, err := unifiedServerBase(data)
	if err != nil {
		t.Fatalf("unifiedServerBase: %v", err)
	}
	if base.addr != ":8443" {
		t.Errorf("default addr: %q", base.addr)
	}
	if len(base.watchDirs) != 1 || base.watchDirs[0].Dir != "just/this" {
		t.Errorf("watchDirs: %+v", base.watchDirs)
	}
}

// server 段独立配置时靠显式 watch_id 注册单根(与 client 的 backend.watch_id 对齐)。
func TestUnifiedServerSingleRootWatchID(t *testing.T) {
	data := []byte(`
server:
  addr: ":8443"
  watch_root: /home/devpod/storage
  watch_id: storage
  auth:
    tokens: ["tok-a"]
`)
	base, err := unifiedServerBase(data)
	if err != nil {
		t.Fatalf("unifiedServerBase: %v", err)
	}
	if len(base.watchDirs) != 1 {
		t.Fatalf("want 1 root watch, got %d", len(base.watchDirs))
	}
	if base.watchDirs[0].ID != "storage" || base.watchDirs[0].Dir != "/home/devpod/storage" {
		t.Errorf("root watch: %+v", base.watchDirs[0])
	}
	if !reflect.DeepEqual(base.tokens, []string{"tok-a"}) {
		t.Errorf("tokens: %v", base.tokens)
	}
}
