package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name       string            `yaml:"name"`
	Version    int               `yaml:"version"`
	Backend    BackendConfig     `yaml:"backend"`
	Workspaces []WorkspaceConfig `yaml:"workspaces"`
	Server     *ServerConfig     `yaml:"server,omitempty"` // 仅 relay server 读取;其它端忽略
	Interval   int               `yaml:"interval_seconds"`
}

// ServerConfig relay server(中转)段。仅在最终实现 relay server 读取的一份统一 config 里使用。
type ServerConfig struct {
	Addr          string          `yaml:"addr"`
	WatchRoot     string          `yaml:"watch_root"` // 中转存储根;各 watch 的 watch_dir 相对它
	Auth          ServerAuth      `yaml:"auth"`
	TLS           ServerTLSConfig `yaml:"tls"`
	TunnelEnabled bool            `yaml:"tunnel_enabled"` // 隧道通道:默认 false,显式开启才放行 MsgTunnel*
	MaxTunnels    int             `yaml:"max_tunnels"`    // 并发隧道上限(<=0 用默认 256),防 token 持有者无界开隧道
}

type ServerAuth struct {
	Tokens []string `yaml:"tokens"`
}

type ServerTLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// WorkspaceConfig 一份「作业配置」:定义在什么目录上、对哪些文件、跑哪些 job。
// 它只描述“做什么”与“在哪听”,并不等于某台执行器。Executor 字段把这份配置绑定到
// 一台具体的执行器(其值即执行器侧 backend.config.executor_id / 注册身份);留空 = 单根回退。
type WorkspaceConfig struct {
	ID          string        `yaml:"id"`
	WatchDir    string        `yaml:"watch_dir"`
	LocalDir    string        `yaml:"local_dir"`
	Paths       []string      `yaml:"paths"`
	Jobs        []JobConfig   `yaml:"jobs"`
	AutoCleanup bool          `yaml:"auto_cleanup"`
	TTL         time.Duration `yaml:"ttl"`                // 仅 server 端使用(中转自动清理)
	Executor    string        `yaml:"executor,omitempty"` // 绑定到哪个执行器(其自身 executor_id);空=单根
}

type BackendConfig struct {
	Type   string                 `yaml:"type"`
	Config map[string]interface{} `yaml:"config"`
}

type JobConfig struct {
	ID      string `yaml:"id"`
	Type    string `yaml:"type"` // 仅 exec;删除统一用 exec(如 `rm -f {file_path}`)
	Cmd     string `yaml:"cmd,omitempty"`
	Cwd     string `yaml:"cwd,omitempty"`
	If      string `yaml:"if,omitempty"`
	Timeout int    `yaml:"timeout,omitempty"` // seconds, optional per-job timeout
}

// ExpandHome expands a leading "~" in path to the user's home directory and
// returns the absolute filesystem path. Paths without a leading "~" are
// returned unchanged. This exists so file operations downstream (e.g. the
// watcher's config backup during sync) get a real path rather than a literal
// "~", which os.Open/os.WriteFile do not expand.
func ExpandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, path[2:]), nil
}

func Load(path string) (*Config, error) {
	expanded, err := ExpandHome(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	return loadFromBytes(data)
}

// LoadFromBytes parses YAML content and applies defaults/normalization.
// This is used for config validation without file I/O (e.g., hot reload).
func LoadFromBytes(data []byte) (*Config, error) {
	// Only expand $VAR and ${VAR} patterns, not {var} patterns
	data = []byte(os.ExpandEnv(string(data)))
	return loadFromBytes(data)
}

// loadFromBytes is the internal implementation shared by Load and LoadFromBytes.
func loadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Interval == 0 {
		cfg.Interval = 60
	}

	// Normalize MSYS-style paths (e.g. /d/...) when running on Windows
	for i := range cfg.Workspaces {
		cfg.Workspaces[i].LocalDir = NormalizeWindowsPath(cfg.Workspaces[i].LocalDir)
		for j := range cfg.Workspaces[i].Jobs {
			if cfg.Workspaces[i].Jobs[j].Cwd != "" {
				cfg.Workspaces[i].Jobs[j].Cwd = NormalizeWindowsPath(cfg.Workspaces[i].Jobs[j].Cwd)
			}
		}
	}

	return &cfg, nil
}

// ApplyConfigFile 校验并原子写回一份新配置到 configPath,覆盖前先备份旧文件到
// configPath+".bak"。由文件命令交换(watcher)与 relay WS 流式 config-sync 两条 sync
// 通道共用,避免复制"校验+备份+tmp/rename 原子写"逻辑。configPath 为空时拒绝。
func ApplyConfigFile(payload []byte, configPath string) error {
	if configPath == "" {
		return errors.New("config path not set")
	}
	if _, err := LoadFromBytes(payload); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cur, err := os.ReadFile(configPath); err == nil {
		if err := os.WriteFile(configPath+".bak", cur, 0644); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
	}
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic replace config: %w", err)
	}
	return nil
}

// ExecutorOwnedKeys 是执行方自身身份/角色/网络相关的 backend.config 键,config-sync
// 不得覆盖,应从执行方当前落盘配置保留。本地(协调方)配置缺这些字段无碍;执行方缺了
// 会导致注册失败——如 `executor: true`(角色开关,缺则 enableExecutor 不执行)与
// `executor_id`(注册身份)被覆盖掉最典型。
var ExecutorOwnedKeys = []string{
	"executor_id", "watch_dir", "command_dir", "executor",
	"executor_dir", "config_path", "network_allow", "headers",
}

// MergeConfigPreservingIdentity 把 incoming 配置叠加到 configPath 的当前配置上,其中
// backend.config 里属于 ExecutorOwnedKeys 的键从当前配置保留,其余(workspaces / name /
// version / url / token / 其它 backend.config 键)取 incoming。返回合并后的 YAML 字节,
// 供后续 ApplyConfigFile 原子写回。这是两条 config-sync 通道(relay WS 与 watcher 命令
// 文件)共用的接缝,修复"本地直推会把执行器身份覆写掉"的问题。
//
// 合并行为:当前文件不存在或不可解析 → 原样返回 incoming(退化为整体覆写,无身份可保留);
// incoming 不可解析 → 返回 error(拒绝写,与现有校验一致);owned 键只在当前文件存在时
// 保留(base 胜出),绝不凭空造键;其它 incoming 键/结构一律不删。
func MergeConfigPreservingIdentity(incoming []byte, configPath string) ([]byte, error) {
	var inc yaml.Node
	if err := yaml.Unmarshal(incoming, &inc); err != nil {
		return nil, fmt.Errorf("parse incoming config: %w", err)
	}

	baseBytes, err := os.ReadFile(configPath)
	if err != nil {
		return incoming, nil // 无当前文件可保留 → 退化为整体覆写
	}
	var base yaml.Node
	if err := yaml.Unmarshal(baseBytes, &base); err != nil {
		return incoming, nil // 当前文件不可解析 → 同样退化为整体覆写
	}

	incRoot := nodeRoot(&inc)
	baseRoot := nodeRoot(&base)
	baseBackend := mapKey(baseRoot, "backend")
	if baseBackend == nil || baseBackend.Kind != yaml.MappingNode {
		return incoming, nil // 当前配置没有 backend 段可保留
	}
	baseCfg := mapKey(baseBackend, "config")
	if baseCfg == nil || baseCfg.Kind != yaml.MappingNode {
		return incoming, nil
	}

	// incoming 无 backend.config 时补一个空 mapping,好让身份字段有地方落。
	incBackend := mapKey(incRoot, "backend")
	if incBackend == nil {
		incBackend = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		incRoot.Content = append(incRoot.Content, keyNode("backend"), incBackend)
	} else if incBackend.Kind != yaml.MappingNode {
		return incoming, nil // incoming backend 非映射,无从合并,退化为整体覆写
	}
	incCfg := mapKey(incBackend, "config")
	if incCfg == nil {
		incCfg = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		incBackend.Content = append(incBackend.Content, keyNode("config"), incCfg)
	} else if incCfg.Kind != yaml.MappingNode {
		return incoming, nil
	}

	for _, k := range ExecutorOwnedKeys {
		baseVal := mapKey(baseCfg, k)
		if baseVal == nil {
			continue // 当前文件无此键,不凭空造
		}
		repl := cloneNode(baseVal)
		if i, ok := mapIndex(incCfg, k); ok {
			incCfg.Content[i+1] = repl
		} else {
			incCfg.Content = append(incCfg.Content, keyNode(k), repl)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&inc); err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return buf.Bytes(), nil
}

// nodeRoot 返回文档的根映射节点(剥掉 DocumentNode)。
func nodeRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

// mapKey 在映射节点中取 key 对应的值节点;m 非映射或找不到返回 nil。
func mapKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapIndex 返回 key 在映射节点 Content 中的下标(i 指向键),命中则 i+1 为值节点。
func mapIndex(m *yaml.Node, key string) (int, bool) {
	if m == nil || m.Kind != yaml.MappingNode {
		return 0, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, true
		}
	}
	return 0, false
}

// keyNode 构造一个 yaml.v3 的标量键节点。
func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

// cloneNode 深度拷贝 yaml.Node,保证切片/映射等嵌套结构原样往返,且不与其他站点共享节点。
func cloneNode(n *yaml.Node) *yaml.Node {
	c := &yaml.Node{
		Kind:        n.Kind,
		Tag:         n.Tag,
		Value:       n.Value,
		Style:       n.Style,
		Anchor:      n.Anchor,
		Alias:       n.Alias,
		LineComment: n.LineComment,
		HeadComment: n.HeadComment,
		FootComment: n.FootComment,
	}
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i := range n.Content {
			c.Content[i] = cloneNode(n.Content[i])
		}
	}
	return c
}

func (c *Config) GetBackendType() string {
	return c.Backend.Type
}

func (c *Config) GetWorkspaceByID(id string) (*WorkspaceConfig, error) {
	for i := range c.Workspaces {
		if c.Workspaces[i].ID == id {
			return &c.Workspaces[i], nil
		}
	}
	return nil, fmt.Errorf("workspace not found: %s", id)
}

// GetWorkspacesByExecutor 返回所有显式绑定到给定执行器(其值即后端 executor_id)的 workspace。
// 执行器侧用它判断“这份 workspace 的 jobs 该由我执行”;无匹配返回 nil。
func (c *Config) GetWorkspacesByExecutor(executorID string) []*WorkspaceConfig {
	var out []*WorkspaceConfig
	for i := range c.Workspaces {
		if c.Workspaces[i].Executor == executorID {
			out = append(out, &c.Workspaces[i])
		}
	}
	return out
}

func NormalizeWindowsPath(p string) string {
	if p == "" || runtime.GOOS != "windows" {
		return p
	}
	// Convert MSYS-style /d/... to D:\...
	if len(p) >= 3 && p[0] == '/' && isAlpha(p[1]) && (p[2] == '/' || p[2] == '\\') {
		drive := strings.ToUpper(string(p[1]))
		rest := p[2:]
		return filepath.FromSlash(drive + ":" + rest)
	}
	return p
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
