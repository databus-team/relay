package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name     string          `yaml:"name"`
	Version  int             `yaml:"version"`
	Backend  BackendConfig   `yaml:"backend"`
	Auth     *AuthConfig     `yaml:"auth,omitempty"`
	Devices  []DeviceConfig  `yaml:"devices"`
	Watchers []WatcherConfig `yaml:"watchers"`
	Interval int             `yaml:"interval_seconds"`
}

type DeviceConfig struct {
	ID      string        `yaml:"id"`
	Name    string        `yaml:"name"`
	Backend BackendConfig `yaml:"backend"`
	Paths   DevicePaths   `yaml:"paths"`
}

type DevicePaths struct {
	WatchDir  string `yaml:"watch_dir"`
	RemoteDir string `yaml:"remote_dir"`
	CommandDir string `yaml:"command_dir"`
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

type WatcherConfig struct {
	Name   string        `yaml:"name"`
	Device string        `yaml:"device"` // device ID to watch
	On     OnConfig      `yaml:"on"`
	Jobs   []JobConfig   `yaml:"jobs"`
}

type OnConfig struct {
	FileMatch string `yaml:"file_match"`
	WatchDir  string `yaml:"watch_dir"`
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

func (c *Config) GetWatchDir(watcherIndex int) string {
	if watcherIndex < 0 || watcherIndex >= len(c.Watchers) {
		return ""
	}
	return c.Watchers[watcherIndex].On.WatchDir
}

func (c *Config) GetCommandDir() string {
	if c.Backend.Config == nil {
		return "/commands"
	}
	if dir, ok := c.Backend.Config["command_dir"].(string); ok {
		return dir
	}
	return "/commands"
}

func (c *Config) GetDevice(deviceID string) (*DeviceConfig, error) {
	for i := range c.Devices {
		if c.Devices[i].ID == deviceID {
			return &c.Devices[i], nil
		}
	}
	return nil, fmt.Errorf("device not found: %s", deviceID)
}

func (c *Config) GetDeviceBackend(deviceID string) (BackendConfig, error) {
	device, err := c.GetDevice(deviceID)
	if err != nil {
		return BackendConfig{}, err
	}
	return device.Backend, nil
}

func (c *Config) GetDevicePaths(deviceID string) (*DevicePaths, error) {
	device, err := c.GetDevice(deviceID)
	if err != nil {
		return nil, err
	}
	return &device.Paths, nil
}

func (c *Config) ListDevices() []DeviceConfig {
	return c.Devices
}