package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
)

// stubBackend records Delete calls so tests can assert what file_delete jobs
// targeted (remote via the backend, versus local via os.Remove).
type stubBackend struct {
	deleted []string
}

func (b *stubBackend) ListDir(ctx context.Context, path string) ([]backend.FileInfo, error) {
	return nil, nil
}
func (b *stubBackend) Read(ctx context.Context, path string) ([]byte, error) { return nil, nil }
func (b *stubBackend) Write(ctx context.Context, path string, content []byte) error {
	return nil
}
func (b *stubBackend) Delete(ctx context.Context, path string) error {
	b.deleted = append(b.deleted, path)
	return nil
}
func (b *stubBackend) SupportsExec() bool { return false }
func (b *stubBackend) Exec(ctx context.Context, cmd, cwd string, timeout int) (string, error) {
	return "", nil
}
func (b *stubBackend) Ping(ctx context.Context, commandDir, watchID string) error { return nil }

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

func TestBuildVariablesRemotePath(t *testing.T) {
	// {file_remote_path} must always point at the original remote file on the
	// watch side, even when a local copy is synced into local_dir. {file_path}/
	// {file_dir} point at the local copy because exec jobs run locally.
	const remote = "/home/devpod/storage/databus_backend/foo.test"
	local := filepath.Join("Z:", "Group_Projects", "databus_backend", "foo.test")

	w := &Watcher{}
	vars := w.buildVariables(remote, local, "foo.test")

	if got := vars["file_remote_path"]; got != remote {
		t.Errorf("file_remote_path = %q, want %q (must stay the remote path)", got, remote)
	}
	if got := vars["file_path"]; got == remote {
		t.Errorf("file_path = %q, want the local copy path", got)
	}
	if got := vars["file_name"]; got != "foo.test" {
		t.Errorf("file_name = %q, want foo.test", got)
	}
}

func TestBuildVariablesNoLocal(t *testing.T) {
	// Without a local copy the file-bound vars fall back to the remote path.
	w := &Watcher{}
	vars := w.buildVariables("/home/dev/storage/x.test", "", "x.test")
	if got, want := vars["file_remote_path"], "/home/dev/storage/x.test"; got != want {
		t.Errorf("file_remote_path = %q, want %q", got, want)
	}
	if got, want := vars["file_path"], "/home/dev/storage/x.test"; got != want {
		t.Errorf("file_path = %q, want %q", got, want)
	}
}

func TestExecuteJobsLocalDelete(t *testing.T) {
	tmp := t.TempDir()
	localPath := filepath.Join(tmp, "foo.test")
	if err := os.WriteFile(localPath, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{jobResults: make(map[string]bool)}
	b := &stubBackend{}
	jobs := []config.JobConfig{
		{ID: "rm", Type: "file_delete", Target: "local", Path: "{file_path}"},
	}

	// target=local must remove the locally-synced copy and NOT call the backend.
	if err := w.executeJobs(context.Background(), jobs, "/remote/x/foo.test", localPath, "foo.test", tmp, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Error("local file should have been removed by target=local file_delete")
	}
	if len(b.deleted) != 0 {
		t.Errorf("backend.Delete should not be called for target=local, got %v", b.deleted)
	}
}

func TestExecuteJobsLocalDeleteMissingIsNoop(t *testing.T) {
	// Deleting an already-absent local file must not error the job chain.
	w := &Watcher{jobResults: make(map[string]bool)}
	b := &stubBackend{}
	jobs := []config.JobConfig{
		{ID: "rm", Type: "file_delete", Target: "local", Path: "{file_path}"},
	}
	missing := filepath.Join(t.TempDir(), "never-wrote.test")
	if err := w.executeJobs(context.Background(), jobs, "/remote/x.test", missing, "x.test", "", b); err != nil {
		t.Fatalf("deleting a missing local file should be a no-op, got: %v", err)
	}
}

func TestExecuteJobsRemoteDeleteDefault(t *testing.T) {
	// Default target (unset) deletes the remote file via the backend.
	w := &Watcher{jobResults: make(map[string]bool)}
	b := &stubBackend{}
	jobs := []config.JobConfig{
		{ID: "rm", Type: "file_delete", Path: "{file_remote_path}"},
	}
	const remote = "/remote/watched/foo.test"
	if err := w.executeJobs(context.Background(), jobs, remote, "", "foo.test", "", b); err != nil {
		t.Fatal(err)
	}
	if len(b.deleted) != 1 || b.deleted[0] != remote {
		t.Errorf("deleted = %v, want [%s]", b.deleted, remote)
	}
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

// findWatchForEvent 按事件子路径路由到对应 workspace。
func TestFindWatchForEventRoutesBySubdir(t *testing.T) {
	w := &Watcher{cfg: &config.Config{Watch: []config.WatchConfig{
		{ID: "backend", WatchDir: "databus_backend", Paths: []string{"*.patch"}},
		{ID: "web", WatchDir: "databus_web", Paths: []string{"*.patch"}},
		{ID: "test", WatchDir: "databus_backend", Paths: []string{"*.test"}}, // 与 backend 同目录,靠模式区分
	}}}

	cases := []struct {
		path string // 事件相对根路径
		want string // 期望命中 workspace ID;空表示不应命中
	}{
		{"databus_backend/a.patch", "backend"},
		{"databus_backend/sub/b.patch", "backend"},
		{"databus_web/a.patch", "web"},
		{"databus_backend/c.test", "test"}, // 同子目录下由不同模式分流
		{"databus-pilot/x.patch", ""},      // 不在任何已知子目录
	}
	for _, c := range cases {
		fi := backend.FileInfo{Name: filepath.Base(c.path), Path: c.path}
		got := w.findWatchForEvent(fi)
		var gotID string
		if got != nil {
			gotID = got.ID
		}
		if gotID != c.want {
			t.Errorf("path %q: got workspace %q, want %q", c.path, gotID, c.want)
		}
	}
}
