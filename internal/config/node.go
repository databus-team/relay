package config

import (
	"fmt"
	"os"
	"strings"
)

// NodeConfig 定义逻辑对等节点属性与访问控制
type NodeConfig struct {
	ID                 string   `yaml:"id"`                  // 节点全局唯一标识 (默认取 hostname)
	Roles              []string `yaml:"roles"`               // 承担角色: "hub", "agent" (可同时具备双角色)
	AllowedControllers []string `yaml:"allowed_controllers"` // 授权发起调度的控制端白名单 ("*" 或具体 node_id)
}

// NormalizeNode 规范化节点配置，兼容旧版配置并补全默认值
func (c *Config) NormalizeNode() {
	if c.Node == nil {
		c.Node = &NodeConfig{}
	}

	// 1. 推导节点 ID
	if c.Node.ID == "" {
		if execID, ok := c.Backend.Config["executor_id"].(string); ok && execID != "" && execID != "default" {
			c.Node.ID = execID
		} else if c.Server != nil && c.Server.Addr != "" {
			c.Node.ID = "hub"
		} else if host, err := os.Hostname(); err == nil && host != "" {
			c.Node.ID = host
		} else {
			c.Node.ID = "node-default"
		}
	}

	// 2. 推导节点角色 (hub / agent)
	if len(c.Node.Roles) == 0 {
		var roles []string
		if c.Server != nil && c.Server.Addr != "" {
			roles = append(roles, "hub", "agent")
		} else if c.Backend.Type != "" {
			roles = append(roles, "agent")
		}
		if len(roles) == 0 {
			roles = []string{"agent"}
		}
		c.Node.Roles = roles
	}

	// 3. 规范化角色全小写
	for i, r := range c.Node.Roles {
		c.Node.Roles[i] = strings.ToLower(strings.TrimSpace(r))
	}

	// 4. 默认授权控制端白名单 (默认全放行 "*")
	if len(c.Node.AllowedControllers) == 0 {
		c.Node.AllowedControllers = []string{"*"}
	}

	// 5. 若承担 Agent 角色，确保 executor 为 true，且绑定当前节点 ID
	if c.IsAgent() {
		if c.Backend.Config == nil {
			c.Backend.Config = make(map[string]interface{})
		}
		if _, ok := c.Backend.Config["executor"]; !ok {
			c.Backend.Config["executor"] = true
		}
		if _, ok := c.Backend.Config["executor_id"]; !ok || c.Backend.Config["executor_id"] == "" {
			c.Backend.Config["executor_id"] = c.NodeID()
		}
	}

	// 6. 若承担 Hub 且同时承担 Agent，自动补全/纠正本地回环 relay 后端
	if c.IsAgent() && c.IsHub() {
		if c.Backend.Type == "" {
			c.Backend.Type = "relay"
		}
		if _, ok := c.Backend.Config["url"]; !ok || c.Backend.Config["url"] == "" {
			c.Backend.Config["url"] = c.GetLocalHubURL()
		}
		tok, hasTok := c.Backend.Config["token"].(string)
		if !hasTok || tok == "" || strings.HasPrefix(tok, "${") {
			if c.Server != nil && len(c.Server.Auth.Tokens) > 0 {
				c.Backend.Config["token"] = os.ExpandEnv(c.Server.Auth.Tokens[0])
			}
		}
		if _, ok := c.Backend.Config["command_dir"]; !ok {
			c.Backend.Config["command_dir"] = "/tmp/relay-commands"
		}
		if _, ok := c.Backend.Config["network_allow"]; !ok {
			c.Backend.Config["network_allow"] = []string{"*"}
		}
	}
}

// NodeID 获取当前节点标识
func (c *Config) NodeID() string {
	if c.Node != nil && c.Node.ID != "" {
		return c.Node.ID
	}
	return "node-default"
}

// IsHub 检查当前节点是否承担 Hub (信令路由/中转) 角色
func (c *Config) IsHub() bool {
	if c.Node == nil {
		return false
	}
	for _, r := range c.Node.Roles {
		if r == "hub" {
			return true
		}
	}
	return false
}

// IsAgent 检查当前节点是否承担 Agent (算力托管/执行器) 角色
func (c *Config) IsAgent() bool {
	if c.Node == nil {
		return false
	}
	for _, r := range c.Node.Roles {
		if r == "agent" {
			return true
		}
	}
	return false
}

// IsControllerAllowed 检查发起调度的控制端节点是否在白名单中
func (c *Config) IsControllerAllowed(controllerNodeID string) bool {
	if c.Node == nil || len(c.Node.AllowedControllers) == 0 {
		return true
	}
	controllerNodeID = strings.TrimSpace(controllerNodeID)
	for _, allowed := range c.Node.AllowedControllers {
		allowed = strings.TrimSpace(allowed)
		if allowed == "*" || allowed == controllerNodeID {
			return true
		}
	}
	return false
}

// GetLocalHubURL 获取本地运行的 Hub 回环 WebSocket URL (若配置了 Server 且处于本地运行状态)
// 例如: "ws://127.0.0.1:8080/relay" 或 "wss://127.0.0.1:8080/relay"
func (c *Config) GetLocalHubURL() string {
	if c.Server == nil || c.Server.Addr == "" {
		return ""
	}
	port := c.Server.Addr
	if strings.Contains(port, ":") {
		parts := strings.Split(port, ":")
		port = parts[len(parts)-1]
	}
	scheme := "ws"
	if c.Server.TLS.Enabled {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://127.0.0.1:%s/relay", scheme, port)
}
