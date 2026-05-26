package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name     string        `yaml:"name"`
	Version  int           `yaml:"version"`
	Backend  BackendConfig `yaml:"backend"`
	Auth     *AuthConfig   `yaml:"auth,omitempty"`
	Watch    []WatchConfig `yaml:"watch"`
	Interval int           `yaml:"interval_seconds"`
}

type WatchConfig struct {
	ID       string      `yaml:"id"`
	WatchDir string      `yaml:"watch_dir"` // 远端 MCP 目录（中转站）
	LocalDir string      `yaml:"local_dir"` // 本机工作目录（jobs 执行位置）
	Paths    []string    `yaml:"paths"`
	Jobs     []JobConfig `yaml:"jobs"`
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
	Type     string `yaml:"type"` // exec, file_delete
	Cmd      string `yaml:"cmd,omitempty"`
	Cwd      string `yaml:"cwd,omitempty"`
	Path     string `yaml:"path,omitempty"`
	If       string `yaml:"if,omitempty"`
	KeepFile bool   `yaml:"keep_file"`
	Timeout  int    `yaml:"timeout,omitempty"` // seconds, optional per-job timeout
}

func Load(path string) (*Config, error) {
	expanded := path
	if strings.HasPrefix(expanded, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		expanded = filepath.Join(home, expanded[2:])
	}

	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Only expand $VAR and ${VAR} patterns, not {var} patterns
	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Interval == 0 {
		cfg.Interval = 60
	}

	// Normalize MSYS-style paths (e.g. /d/...) when running on Windows
	for i := range cfg.Watch {
		cfg.Watch[i].LocalDir = normalizeWindowsPath(cfg.Watch[i].LocalDir)
		for j := range cfg.Watch[i].Jobs {
			if cfg.Watch[i].Jobs[j].Cwd != "" {
				cfg.Watch[i].Jobs[j].Cwd = normalizeWindowsPath(cfg.Watch[i].Jobs[j].Cwd)
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

func normalizeWindowsPath(p string) string {
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
