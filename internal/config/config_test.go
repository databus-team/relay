package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `
name: file-exchange
version: 1

backend:
  type: local
  config:
    base_dir: /tmp
    command_dir: /commands

watch:
  - id: test
    watch_dir: /tmp
    paths: ["*.txt"]
    local_dir: /tmp
    jobs:
      - id: test
        type: exec
        cmd: echo {file_name}
        cwd: /tmp

interval_seconds: 60
`

	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Name != "file-exchange" {
		t.Errorf("Expected name 'file-exchange', got '%s'", cfg.Name)
	}
	if cfg.Version != 1 {
		t.Errorf("Expected version 1, got %d", cfg.Version)
	}
	if cfg.Backend.Type != "local" {
		t.Errorf("Expected backend type 'local', got '%s'", cfg.Backend.Type)
	}
	if len(cfg.Watch) != 1 {
		t.Errorf("Expected 1 watcher, got %d", len(cfg.Watch))
	}
	if cfg.Watch[0].ID != "test" {
		t.Errorf("Expected watcher id 'test', got '%s'", cfg.Watch[0].ID)
	}
}
