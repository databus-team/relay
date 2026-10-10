package config

import (
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
			roles = append(roles, "hub")
		}
		if c.Backend.Type != "" {
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
