package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/user/relay/internal/config"
)

// ----- resolveWorkspaceID unit tests -----

func TestResolveWorkspaceID_ExplicitWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{{ID: "b"}}
	got, err := resolveWorkspaceID(cfg, "b")
	if err != nil || got != "b" {
		t.Fatalf("got %q, err %v; want explicit value to win", got, err)
	}
}

func TestResolveWorkspaceID_SingleMatchFromCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	name := filepath.Base(dir)

	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{{ID: name, LocalDir: "x"}, {ID: "other", LocalDir: "y"}}

	got, err := resolveWorkspaceID(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != name {
		t.Errorf("got %q, want %q", got, name)
	}
}

func TestResolveWorkspaceID_NoMatchListsAvailable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{{ID: "web", LocalDir: "/w"}, {ID: "api", LocalDir: "/a"}}

	_, err := resolveWorkspaceID(cfg, "")
	if err == nil {
		t.Fatal("expected error for no matching workspace")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "api") {
		t.Errorf("error should list available workspaces: %v", err)
	}
}

func TestResolveWorkspaceID_AmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	name := filepath.Base(dir)

	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{{ID: name, LocalDir: "/first"}, {ID: name, LocalDir: "/second"}}

	_, err := resolveWorkspaceID(cfg, "")
	if err == nil {
		t.Fatal("expected ambiguous-match error")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("expected ambiguity error, got: %v", err)
	}
}

// ----- pull --delete integration test (local backend) -----

func TestPull_DeleteRemovesRemoteFile(t *testing.T) {
	// Remote exchange dir inside a temp base dir.
	base := t.TempDir()
	watchDir := "patches"
	remoteDir := filepath.Join(base, watchDir)
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	remoteFile := filepath.Join(remoteDir, "a.patch")
	if err := os.WriteFile(remoteFile, []byte("patch"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgContent := "name: relay\nversion: 1\nbackend:\n  type: local\n  config:\n    base_dir: " + base + "\n" +
		"watch:\n  - id: demo\n    watch_dir: " + watchDir + "\n    local_dir: /tmp\n    paths: [\"*.patch\"]\ninterval_seconds: 60\n"
	_, cleanup := withTempConfig(t, cfgContent)
	defer cleanup()

	// Pull into a fresh output dir.
	out := t.TempDir()
	t.Chdir(out)

	*pullWatch = "demo"
	*pullFile = "a.patch"
	*pullDelete = true

	runPull()

	local := filepath.Join(out, "a.patch")
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("expected local file pulled: %v", err)
	}
	if _, err := os.Stat(remoteFile); !os.IsNotExist(err) {
		t.Errorf("expected remote file deleted, stat err = %v", err)
	}
}

func TestPull_NoDeleteKeepsRemote(t *testing.T) {
	base := t.TempDir()
	watchDir := "patches"
	remoteDir := filepath.Join(base, watchDir)
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	remoteFile := filepath.Join(remoteDir, "a.patch")
	if err := os.WriteFile(remoteFile, []byte("patch"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgContent := "name: relay\nversion: 1\nbackend:\n  type: local\n  config:\n    base_dir: " + base + "\n" +
		"watch:\n  - id: demo\n    watch_dir: " + watchDir + "\n    local_dir: /tmp\n    paths: [\"*.patch\"]\ninterval_seconds: 60\n"
	_, cleanup := withTempConfig(t, cfgContent)
	defer cleanup()

	out := t.TempDir()
	t.Chdir(out)

	*pullWatch = "demo"
	*pullFile = "a.patch"
	*pullDelete = false

	runPull()

	if _, err := os.Stat(remoteFile); err != nil {
		t.Errorf("expected remote file kept without --delete: %v", err)
	}
}
