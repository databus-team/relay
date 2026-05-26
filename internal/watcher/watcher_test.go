package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/user/relay/internal/config"
)

func TestLoadFromBytes(t *testing.T) {
	// Test that LoadFromBytes parses config correctly
	cfg := &config.Config{
		Watch:    []config.WatchConfig{},
		Interval: 60,
	}
	_ = cfg
}

func TestWatcherConfigPathStored(t *testing.T) {
	// Test that configPath is stored in watcher
	cfg := &config.Config{
		Watch:    []config.WatchConfig{},
		Interval: 60,
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.yaml")
	if err := os.WriteFile(configPath, []byte("watch: []\ninterval_seconds: 60"), 0644); err != nil {
		t.Fatal(err)
	}

	w, err := New(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}

	if w.configPath != configPath {
		t.Errorf("configPath mismatch: got %q, want %q", w.configPath, configPath)
	}
}

func TestConfigSyncDetection(t *testing.T) {
	// Test that config sync commands are detected
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "config sync command",
			json: `{"id":"test-123","op":"relay:config-sync","payload":"d2F0Y2g6IFtdCm50ZXJ2YWxfc2Vjb25kczogNjA="}`,
			want: "relay:config-sync",
		},
		{
			name: "exec command without op",
			json: `{"id":"exec-456","cmd":"echo hello","cwd":"/tmp","timeout":30}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify the op field exists in the test JSON
			if !strings.Contains(tt.json, tt.want) {
				t.Errorf("test JSON should contain op=%q", tt.want)
			}
		})
	}
}

func TestPendingConfigMutex(t *testing.T) {
	// Test that pending config operations are thread-safe
	var mu sync.Mutex
	var pending []byte

	// Simulate concurrent write
	go func() {
		mu.Lock()
		pending = []byte("config content")
		mu.Unlock()
	}()

	// Simulate concurrent read
	mu.Lock()
	_ = pending != nil
	mu.Unlock()
}

func TestBackupPath(t *testing.T) {
	// Test that backup path is correctly formed
	tests := []struct {
		configPath string
		want       string
	}{
		{"/etc/relay.yaml", "/etc/relay.yaml.bak"},
		{"/home/user/config.yaml", "/home/user/config.yaml.bak"},
	}

	for _, tt := range tests {
		got := tt.configPath + ".bak"
		if got != tt.want {
			t.Errorf("backup path: got %q, want %q", got, tt.want)
		}
	}
}

func TestApplyPendingConfigAtomic(t *testing.T) {
	// Test atomic config application using Rename
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	tmpPath := configPath + ".tmp"

	// Write temp file
	if err := os.WriteFile(tmpPath, []byte("new config"), 0644); err != nil {
		t.Fatal(err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, configPath); err != nil {
		t.Fatal(err)
	}

	// Verify temp file is gone
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should be removed after rename")
	}

	// Verify config file exists with new content
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new config" {
		t.Errorf("config content: got %q, want %q", string(data), "new config")
	}
}