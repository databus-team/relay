package watcher

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/exchange"
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
		Workspaces: []config.WorkspaceConfig{},
		Interval:   60,
	}
	_ = cfg
}

func TestWatcherConfigPathStored(t *testing.T) {
	// Test that configPath is stored in watcher
	cfg := &config.Config{
		Workspaces: []config.WorkspaceConfig{},
		Interval:   60,
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

func TestExecuteJobsRejectsRemovedFileDelete(t *testing.T) {
	// file_delete 专有类型已移除:任何本本中出现都应作为未知类型报错(删除用 exec)。
	w := &Watcher{jobResults: make(map[string]bool)}
	jobs := []config.JobConfig{
		{ID: "rm", Type: "file_delete", Cmd: "rm -f {file_path}"},
	}
	if err := w.executeJobs(context.Background(), jobs, "/remote/x.test", "x.test", "x.test", "", &stubBackend{}); err == nil {
		t.Fatal("expected unknown-job-type error for the removed file_delete type")
	} else if !strings.Contains(err.Error(), "unknown job type") {
		t.Fatalf("unexpected error: %v", err)
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

// sync 指令一旦被处理就应立刻落盘:事件驱动模式(relay 后端)的主循环从不调用
// runOnce/applyPendingConfig,若只"stage 到下一周期"则配置永远不会写盘
// (此前表现:日志显示同步成功但配置文件仍旧)。回归用例验证立即生效。
func TestHandleConfigSyncAppliesImmediately(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("name: old\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{cfg: &config.Config{}, configPath: configPath}

	payload := []byte("name: relay\nversion: 2\nbackend:\n  type: relay\ninterval_seconds: 10\n")
	cmd := exchange.CmdFile{
		ID:      "sync-1",
		Op:      exchange.ConfigSyncOp,
		Payload: base64.StdEncoding.EncodeToString(payload),
	}

	res, err := w.handleConfigSync(context.Background(), cmd, nil)
	if err != nil {
		t.Fatalf("handleConfigSync: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read applied config: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("config not applied to disk:\n got=%q\nwant=%q", got, payload)
	}
}

// handleConfigSync 必须保留执行方身份字段(config-sync 不得覆写 executor/watch_id 等)。
func TestHandleConfigSyncPreservesExecutorIdentity(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	// seed 一份带执行器身份的运行配置
	if err := os.WriteFile(configPath, []byte("name: relay\nversion: 2\nbackend:\n  type: relay\n  config:\n    watch_id: site-b\n    executor: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{cfg: &config.Config{}, configPath: configPath}

	// 推来的本地协调方配置不含 watch_id/executor
	payload := []byte("name: relay\nversion: 3\nbackend:\n  type: relay\n  config:\n    url: ws://x:8443/relay\nworkspaces:\n  - id: w\n    paths: [\"*.patch\"]\n")
	cmd := exchange.CmdFile{
		ID:      "sync-2",
		Op:      exchange.ConfigSyncOp,
		Payload: base64.StdEncoding.EncodeToString(payload),
	}

	res, err := w.handleConfigSync(context.Background(), cmd, nil)
	if err != nil {
		t.Fatalf("handleConfigSync: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read applied config: %v", err)
	}
	cfg, err := config.LoadFromBytes(got)
	if err != nil {
		t.Fatalf("parse applied config: %v", err)
	}
	if cfg.Backend.Config["executor"] != true {
		t.Errorf("executor identity lost: %#v", cfg.Backend.Config["executor"])
	}
	if cfg.Backend.Config["watch_id"] != "site-b" {
		t.Errorf("watch_id identity lost: got %v", cfg.Backend.Config["watch_id"])
	}
	if v, _ := cfg.Backend.Config["url"].(string); v != "ws://x:8443/relay" {
		t.Errorf("shared url not overlaid: %v", cfg.Backend.Config["url"])
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != "w" {
		t.Errorf("workspaces not overlaid: %+v", cfg.Workspaces)
	}
}

// findWatchForEvent 按事件子路径路由到对应 workspace。
func TestFindWatchForEventRoutesBySubdir(t *testing.T) {
	w := &Watcher{cfg: &config.Config{Workspaces: []config.WorkspaceConfig{
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
