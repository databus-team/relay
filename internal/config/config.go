package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name     string           `yaml:"name"`
	Version  int              `yaml:"version"`
	Backend  BackendConfig    `yaml:"backend"`
	Auth     *AuthConfig      `yaml:"auth,omitempty"`
	Projects []ProjectConfig  `yaml:"projects"`
	Interval int              `yaml:"interval_seconds"`
}

type ProjectConfig struct {
	ID       string        `yaml:"id"`
	Name     string        `yaml:"name"`
	WatchDir string       `yaml:"watch_dir"`
	FileMatch string      `yaml:"file_match"`
	Jobs     []JobConfig  `yaml:"jobs"`
}

type BackendConfig struct {
	Type   string                 `yaml:"type"`
	Config map[string]interface{} `yaml:"config"`
}

type AuthConfig struct {
	Method           string `yaml:"method"`
	LoginURL         string `yaml:"login_url"`
	TokenCookieName  string `yaml:"token_cookie_name"`
	ProxyPort        int    `yaml:"proxy_port"`
	TokenCacheFile   string `yaml:"token_cache_file"`
}

type JobConfig struct {
	ID       string `yaml:"id"`
	Type     string `yaml:"type"` // exec, file_delete
	Cmd      string `yaml:"cmd,omitempty"`
	Cwd      string `yaml:"cwd,omitempty"`
	Path     string `yaml:"path,omitempty"`
	If       string `yaml:"if,omitempty"`
	KeepFile bool   `yaml:"keep_file"`
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
	// to avoid conflict with our built-in variables
	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Interval == 0 {
		cfg.Interval = 60
	}

	return &cfg, nil
}

func (c *Config) GetBackendType() string {
	return c.Backend.Type
}

func (c *Config) GetProjectByID(projectID string) (*ProjectConfig, error) {
	for i := range c.Projects {
		if c.Projects[i].ID == projectID {
			return &c.Projects[i], nil
		}
	}
	return nil, fmt.Errorf("project not found: %s", projectID)
}

func (c *Config) ListProjects() []ProjectConfig {
	return c.Projects
}