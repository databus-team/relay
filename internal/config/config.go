package config

import (
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
	Auth       *AuthConfig       `yaml:"auth,omitempty"`
	Workspaces []WorkspaceConfig `yaml:"workspaces"`
	Server     *ServerConfig     `yaml:"server,omitempty"` // 仅 relay server 读取;其它端忽略
	Interval   int               `yaml:"interval_seconds"`
}

// ServerConfig relay server(中转)段。仅在最终实现 relay server 读取的一份统一 config 里使用。
type ServerConfig struct {
	Addr          string          `yaml:"addr"`
	WatchRoot     string          `yaml:"watch_root"` // 中转存储根;各 watch 的 watch_dir 相对它
	WatchID       string          `yaml:"watch_id"`   // 单根模式下此 watch id(= client 的 backend.config.watch_id)
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
// 一台具体的执行器(其值即执行器侧 backend.config.watch_id / 注册身份);留空 = 单根回退。
type WorkspaceConfig struct {
	ID          string        `yaml:"id"`
	WatchDir    string        `yaml:"watch_dir"`
	LocalDir    string        `yaml:"local_dir"`
	Paths       []string      `yaml:"paths"`
	Jobs        []JobConfig   `yaml:"jobs"`
	AutoCleanup bool          `yaml:"auto_cleanup"`
	TTL         time.Duration `yaml:"ttl"`                // 仅 server 端使用(中转自动清理)
	Executor    string        `yaml:"executor,omitempty"` // 绑定到哪个执行器(其自身 watch_id);空=单根
}

type BackendConfig struct {
	Type   string                 `yaml:"type"`
	Config map[string]interface{} `yaml:"config"`
}

type AuthConfig struct {
	Method          string `yaml:"method"`
	LoginURL        string `yaml:"login_url"`
	TokenCookieName string `yaml:"token_cookie_name"`
	ProxyPort       int    `yaml:"proxy_port"`
	TokenCacheFile  string `yaml:"token_cache_file"`
}

type JobConfig struct {
	ID       string `yaml:"id"`
	Type     string `yaml:"type"` // 仅 exec;删除统一用 exec(如 `rm -f {file_path}`)
	Cmd      string `yaml:"cmd,omitempty"`
	Cwd      string `yaml:"cwd,omitempty"`
	If       string `yaml:"if,omitempty"`
	KeepFile bool   `yaml:"keep_file"`
	Timeout  int    `yaml:"timeout,omitempty"` // seconds, optional per-job timeout
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

// GetWorkspacesByExecutor 返回所有显式绑定到给定执行器(其值即后端 watch_id)的 workspace。
// 执行器侧用它判断“这份 workspace 的 jobs 该由我执行”;无匹配返回 nil。
func (c *Config) GetWorkspacesByExecutor(watchID string) []*WorkspaceConfig {
	var out []*WorkspaceConfig
	for i := range c.Workspaces {
		if c.Workspaces[i].Executor == watchID {
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
