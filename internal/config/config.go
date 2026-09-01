package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name     string        `yaml:"name"`
	Version  int           `yaml:"version"`
	Backend  BackendConfig `yaml:"backend"`
	Auth     *AuthConfig   `yaml:"auth,omitempty"`
	Watch    []WatchConfig `yaml:"watch"`
	Server   *ServerConfig `yaml:"server,omitempty"` // 仅 relay server 读取;其它端忽略
	Interval int           `yaml:"interval_seconds"`
}

// ServerConfig relay server(中转)段。仅在最终实现 relay server 读取的一份统一 config 里使用。
type ServerConfig struct {
	Addr      string          `yaml:"addr"`
	WatchRoot string          `yaml:"watch_root"` // 中转存储根;各 watch 的 watch_dir 相对它
	WatchID   string          `yaml:"watch_id"`   // 单根模式下此 watch id(= client 的 backend.config.watch_id)
	Auth      ServerAuth      `yaml:"auth"`
	TLS       ServerTLSConfig `yaml:"tls"`
}

type ServerAuth struct {
	Tokens []string `yaml:"tokens"`
}

type ServerTLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type WatchConfig struct {
	ID          string        `yaml:"id"`
	WatchDir    string        `yaml:"watch_dir"`
	LocalDir    string        `yaml:"local_dir"`
	Paths       []string      `yaml:"paths"`
	Jobs        []JobConfig   `yaml:"jobs"`
	AutoCleanup bool          `yaml:"auto_cleanup"`
	TTL         time.Duration `yaml:"ttl"` // 仅 server 端使用(中转自动清理)
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
	Type     string `yaml:"type"`             // exec, file_delete
	Target   string `yaml:"target,omitempty"` // file_delete only: remote (default) | local
	Cmd      string `yaml:"cmd,omitempty"`
	Cwd      string `yaml:"cwd,omitempty"`
	Path     string `yaml:"path,omitempty"`
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
	for i := range cfg.Watch {
		cfg.Watch[i].LocalDir = NormalizeWindowsPath(cfg.Watch[i].LocalDir)
		for j := range cfg.Watch[i].Jobs {
			if cfg.Watch[i].Jobs[j].Cwd != "" {
				cfg.Watch[i].Jobs[j].Cwd = NormalizeWindowsPath(cfg.Watch[i].Jobs[j].Cwd)
			}
		}
	}

	return &cfg, nil
}

func (c *Config) GetBackendType() string {
	return c.Backend.Type
}

func (c *Config) GetWatchByID(id string) (*WatchConfig, error) {
	for i := range c.Watch {
		if c.Watch[i].ID == id {
			return &c.Watch[i], nil
		}
	}
	return nil, fmt.Errorf("watch not found: %s", id)
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
