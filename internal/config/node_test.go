package config

import (
	"testing"
)

func TestNodeConfig_NormalizeDefaults(t *testing.T) {
	// 1. 旧版只有 backend 配置 -> 自动推导为 agent 角色
	yamlLegacyAgent := `
name: relay
backend:
  type: relay
  config:
    executor_id: win-office
`
	cfg1, err := LoadFromBytes([]byte(yamlLegacyAgent))
	if err != nil {
		t.Fatalf("LoadFromBytes error: %v", err)
	}
	if cfg1.NodeID() != "win-office" {
		t.Errorf("NodeID = %q, want %q", cfg1.NodeID(), "win-office")
	}
	if !cfg1.IsAgent() {
		t.Errorf("expected IsAgent = true")
	}
	if cfg1.IsHub() {
		t.Errorf("expected IsHub = false")
	}
	if !cfg1.IsControllerAllowed("mac-laptop") {
		t.Errorf("expected all controllers allowed by default")
	}

	// 2. 旧版只有 server 配置 -> 自动推导为 hub 角色
	yamlLegacyHub := `
name: relay
server:
  addr: ":8443"
`
	cfg2, err := LoadFromBytes([]byte(yamlLegacyHub))
	if err != nil {
		t.Fatalf("LoadFromBytes error: %v", err)
	}
	if !cfg2.IsHub() {
		t.Errorf("expected IsHub = true")
	}

	// 3. 显式双角色全对等节点 (兼任 hub 与 agent) 与指定白名单
	yamlDualPeer := `
name: relay
node:
  id: linux-heavy
  roles: ["Hub", "Agent"]
  allowed_controllers: ["mac-laptop", "ci-runner"]
server:
  addr: ":8443"
backend:
  type: relay
`
	cfg3, err := LoadFromBytes([]byte(yamlDualPeer))
	if err != nil {
		t.Fatalf("LoadFromBytes error: %v", err)
	}
	if cfg3.NodeID() != "linux-heavy" {
		t.Errorf("NodeID = %q, want %q", cfg3.NodeID(), "linux-heavy")
	}
	if !cfg3.IsHub() || !cfg3.IsAgent() {
		t.Errorf("expected dual roles (hub & agent), got roles: %v", cfg3.Node.Roles)
	}
	if !cfg3.IsControllerAllowed("mac-laptop") {
		t.Errorf("expected mac-laptop allowed")
	}
	if !cfg3.IsControllerAllowed("ci-runner") {
		t.Errorf("expected ci-runner allowed")
	}
	if cfg3.IsControllerAllowed("untrusted-node") {
		t.Errorf("expected untrusted-node to be denied")
	}
}

func TestGetLocalHubURL(t *testing.T) {
	// 1. 无 TLS
	cfg := &Config{
		Server: &ServerConfig{
			Addr: "0.0.0.0:8080",
		},
	}
	if got := cfg.GetLocalHubURL(); got != "ws://127.0.0.1:8080/ws" {
		t.Errorf("GetLocalHubURL() = %q, want %q", got, "ws://127.0.0.1:8080/ws")
	}

	// 2. 带 TLS
	cfgTLS := &Config{
		Server: &ServerConfig{
			Addr: ":8443",
			TLS: ServerTLSConfig{
				Enabled: true,
			},
		},
	}
	if got := cfgTLS.GetLocalHubURL(); got != "wss://127.0.0.1:8443/ws" {
		t.Errorf("GetLocalHubURL() = %q, want %q", got, "wss://127.0.0.1:8443/ws")
	}

	// 3. 未配置 Server
	cfgNoServer := &Config{}
	if got := cfgNoServer.GetLocalHubURL(); got != "" {
		t.Errorf("GetLocalHubURL() = %q, want empty string", got)
	}
}

