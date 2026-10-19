package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bk "github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/portfwd"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
	"github.com/user/relay/internal/supervisor"
	"github.com/user/relay/internal/sync/git"
)

// withSupervisorBaseDir 把执行方的任务目录接到临时目录,避免用例碰到真实的 ~/.relay/tasks。
// 返回的目录同时是该用例的任务根目录。
func withSupervisorBaseDir(t *testing.T) string {
	t.Helper()
	baseDir := t.TempDir()
	prevDir := supervisorBaseDir
	supervisorBaseDir = baseDir
	t.Cleanup(func() { supervisorBaseDir = prevDir })
	return baseDir
}

// TestNewSupervisor_OnePerBaseDir 覆盖 B1:同一个任务目录在本进程只有一个 supervisor。
// 执行方会反复构造 backend(命令轮询每轮一次、文件事件各一次),每个 backend 各建一份会让同一批
// 任务被反复加载与回写,Cleanup 还会拿着两份互不知情的内存视图对同一批任务各杀一次。
func TestNewSupervisor_OnePerBaseDir(t *testing.T) {
	firstDir := withSupervisorBaseDir(t)
	first, err := newSupervisor()
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	again, err := newSupervisor()
	if err != nil {
		t.Fatalf("new supervisor (同目录第二次): %v", err)
	}
	if first != again {
		t.Errorf("同一任务目录建出了两份 supervisor")
	}

	supervisorsMu.Lock()
	registeredSup, registered := supervisors[firstDir]
	supervisorsMu.Unlock()
	if !registered || registeredSup != first {
		t.Errorf("supervisor 未按目录登记进 supervisors,Cleanup 停不掉它托管的任务")
	}

	// 换一个目录仍然是独立的一份:以目录为键,测试接缝才互不串台。
	otherDir := withSupervisorBaseDir(t)
	if otherDir == firstDir {
		t.Fatalf("临时目录重复: %s", otherDir)
	}
	other, err := newSupervisor()
	if err != nil {
		t.Fatalf("new supervisor (另一目录): %v", err)
	}
	if other == first {
		t.Errorf("不同任务目录复用了同一份 supervisor")
	}
}

func newTestHub(t testing.TB, watchDir string) (*httptest.Server, string) {
	t.Helper()
	srv, err := server.New(server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{"tok-exe", "tok-req"},
		},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
}

func newTestHubWithTunnel(t testing.TB, watchDir string) (*httptest.Server, string) {
	t.Helper()
	srv, err := server.New(server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{"tok-exe", "tok-req"},
		},
		TunnelEnabled: true,
	})
	if err != nil {
		t.Fatalf("create server with tunnel: %v", err)
	}
	ts := httptest.NewServer(srv)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
}

// 端到端:真实执行方(executor)注册到中转,由请求方经中转转发,命令在远端 sh -c 流式执行。
func TestEndToEnd_ExecStream(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	// 执行方:watch 上注册为 executor,true 触发流式 sh -c 执行
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 请求方:只发起 exec,不做 executor
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	rb, ok := reqBackend.(bk.ExecStreamBackend)
	if !ok {
		t.Fatalf("requester backend does not implement ExecStreamBackend")
	}

	// 逐行 echo,验证增量流式输出
	var live []string
	exit, err := rb.ExecStream(ctx, "", "printf 'one\\ntwo\\nthree\\n'", "", 10, func(c bk.ExecChunk) {
		live = append(live, c.Data)
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit code: %d", exit)
	}
	joined := strings.Join(live, "")
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing output %q; got %q", want, joined)
		}
	}
}

// 端到端:请求方经中转把新配置直达执行方,执行方校验+原子落盘到 config_path,回执成功。
func TestEndToEnd_ConfigSync(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)

	watchDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("name: old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	// 执行方:注册为 executor,config_path 指向要落盘的文件
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "executor": true,
		"config_path": configPath,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 请求方:走 config-sync 流程
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	cs, ok := reqBackend.(bk.ConfigSyncCapable)
	if !ok {
		t.Fatalf("requester backend does not implement ConfigSyncCapable")
	}

	payload := []byte("name: relay\nversion: 2\nbackend:\n  type: relay\n")
	exit, err := cs.ConfigSync(ctx, "", payload)
	if err != nil {
		t.Fatalf("config sync: %v", err)
	}
	if exit != 0 {
		t.Fatalf("config sync exit: %d", exit)
	}

	// 执行方应已原子落盘新配置
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read synced config: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("config content:\n got=%q\nwant=%q", got, payload)
	}
	// 旧文件应已备份
	if _, err := os.Stat(configPath + ".bak"); err != nil {
		t.Errorf("backup file not created: %v", err)
	}
}

// 端到端:config-sync 必须保留执行方身份字段(executor/executor_id),不得被本地协调方
// 配置覆写,否则注册失败。
func TestEndToEnd_ConfigSyncPreservesExecutorIdentity(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)

	watchDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	// seed 一份带执行器身份的运行配置
	if err := os.WriteFile(configPath, []byte("name: relay\nversion: 2\nbackend:\n  type: relay\n  config:\n    executor_id: test\n    executor: true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "executor": true,
		"config_path": configPath,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	cs, ok := reqBackend.(bk.ConfigSyncCapable)
	if !ok {
		t.Fatalf("requester backend does not implement ConfigSyncCapable")
	}

	// 推来的本地协调方配置不含 executor_id/executor
	payload := []byte("name: relay\nversion: 3\nbackend:\n  type: relay\n  config:\n    url: ws://x:8443/relay\nworkspaces:\n  - id: w\n    paths: [\"*.patch\"]\n")
	exit, err := cs.ConfigSync(ctx, "", payload)
	if err != nil {
		t.Fatalf("config sync: %v", err)
	}
	if exit != 0 {
		t.Fatalf("config sync exit: %d", exit)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read synced config: %v", err)
	}
	cfg, err := config.LoadFromBytes(got)
	if err != nil {
		t.Fatalf("parse synced config: %v", err)
	}
	if cfg.Backend.Config["executor"] != true {
		t.Errorf("executor identity lost: %#v", cfg.Backend.Config["executor"])
	}
	if cfg.Backend.Config["executor_id"] != "test" {
		t.Errorf("executor_id identity lost: got %v", cfg.Backend.Config["executor_id"])
	}
	if v, _ := cfg.Backend.Config["url"].(string); v != "ws://x:8443/relay" {
		t.Errorf("shared url not overlaid: %v", cfg.Backend.Config["url"])
	}
}

// end-to-end: 请求方 PushJob 直达执行方,目的端落盘并跑本地 job,推送流回。
func TestEndToEnd_PushJob(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	execRoot := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	// 执行方:注册为 executor,配 executor_dir,注册 push-job handler
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	// (handler 由 watcher 注入,直接构造再取下执行方 backend 对象比较麻烦;改为经 client 单测覆盖。)

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	pj, ok := reqBackend.(bk.PushJobSender)
	if !ok {
		t.Fatalf("requester backend does not implement PushJobSender")
	}

	var streamed []string
	exit, err := pj.PushJob(ctx, "", "sub/f.txt", []byte("hello from push\n"), func(c bk.ExecChunk) {
		streamed = append(streamed, c.Data)
	})
	if err != nil {
		t.Fatalf("push job: %v", err)
	}
	if exit != 0 {
		t.Fatalf("push job exit: %d", exit)
	}
	// 无 handler 时应为成功;执行方回传 "[pushed ...]" 提示流
	if len(streamed) == 0 || !strings.Contains(streamed[0], "pushed") {
		t.Errorf("expected pushed note chunk, got %q", streamed)
	}
	// 文件应已落在执行方 execRoot/sub/f.txt
	got, err := os.ReadFile(filepath.Join(execRoot, "sub", "f.txt"))
	if err != nil {
		t.Fatalf("pushed file not present: %v", err)
	}
	if string(got) != "hello from push\n" {
		t.Errorf("content: %q", string(got))
	}
}

// 端到端:请求方 PushNoJobs(转 transport 通道,Jobs=false)把内容纯下发到执行方绝对路径,
// 不触发任何 workspace job(无 "[pushed ...]" 提示流),字节完全一致。
func TestEndToEnd_PushNoJobs(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	execRoot := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	// 执行方:注册为 executor,配 executor_dir。
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	tn, ok := reqBackend.(bk.PushNoJobsSender)
	if !ok {
		t.Fatalf("requester backend does not implement PushNoJobsSender")
	}

	dest := filepath.Join(execRoot, "sub", "relay.new")
	exit, err := tn.PushNoJobs(ctx, "", dest, []byte("deploy blob\n"))
	if err != nil {
		t.Fatalf("push no-jobs: %v", err)
	}
	if exit != 0 {
		t.Fatalf("push no-jobs exit: %d", exit)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("pushed binary not present: %v", err)
	}
	if string(got) != "deploy blob\n" {
		t.Errorf("content: %q", string(got))
	}
}

// 大文件流式直达:请求方 PushJob 分块流式把数 MB 二进制安全落盘到执行方,字节完全一致。
func TestEndEndPushJobLarge(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	execRoot := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	pj := reqBackend.(bk.PushJobSender)

	// ~4 MB 不可压缩随机字节,跨越多个 64KB 分块。
	size := 4 << 20
	blob := make([]byte, size)
	rng := rand.New(rand.NewSource(42))
	rng.Read(blob)

	exit, err := pj.PushJob(ctx, "", "big.bin", blob, func(bk.ExecChunk) {})
	if err != nil {
		t.Fatalf("push job: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit: %d", exit)
	}
	got, err := os.ReadFile(filepath.Join(execRoot, "big.bin"))
	if err != nil {
		t.Fatalf("pushed large file not present: %v", err)
	}
	if len(got) != len(blob) {
		t.Fatalf("size mismatch: got %d want %d", len(got), len(blob))
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("large file content mismatch")
	}
}

// 无执行方时 PushJob 回退:文件落到中转暂存目录,不报错。
func TestEndToEnd_PushJobNoExecutorFallback(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	pj := reqBackend.(bk.PushJobSender)

	var streamed []string
	exit, err := pj.PushJob(ctx, "", "fallback.txt", []byte("staged\n"), func(c bk.ExecChunk) {
		streamed = append(streamed, c.Data)
	})
	if err != nil {
		t.Fatalf("push job: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit: %d", exit)
	}
	found := false
	for _, s := range streamed {
		if strings.Contains(s, "staged") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected staged fallback message, got %q", streamed)
	}
	got, err := os.ReadFile(filepath.Join(watchDir, "fallback.txt"))
	if err != nil {
		t.Fatalf("staged file not present: %v", err)
	}
	if string(got) != "staged\n" {
		t.Errorf("staged content: %q", string(got))
	}
}

// 无 executor 时 exec 报错(中转不兜底本地执行)。
func TestEndToEnd_ExecNoExecutor(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	rb := reqBackend.(bk.ExecStreamBackend)
	if _, err := rb.ExecStream(context.Background(), "", "echo x", "", 3, nil); err == nil {
		t.Error("expected error when no executor, got nil")
	} else if !strings.Contains(err.Error(), "no executor") {
		t.Errorf("unexpected error: %v", err)
	}
}

// 端到端:单个执行方在线时,relay 后端 status 返回 段1 + 该执行方的段2/累计 + 中转版本。
func TestEndToEnd_Status(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	// 执行方:watch 上注册为 executor。
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "test", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	st, err := rb.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Seg1.LatencyMS == nil || *st.Seg1.LatencyMS < 0 {
		t.Errorf("seg1.latency_ms missing: %+v", st.Seg1)
	}
	if st.Transit.Role != "transit" || st.Transit.Version == "" {
		t.Errorf("transit node missing: %+v", st.Transit)
	}
	if len(st.Executors) != 1 {
		t.Fatalf("executors: got %d, want 1: %+v", len(st.Executors), st.Executors)
	}
	e := st.Executors[0]
	if e.ExecutorID != "test" {
		t.Errorf("executor watch: got %q, want %q", e.ExecutorID, "test")
	}
	if e.Seg2.LatencyMS == nil || *e.Seg2.LatencyMS < 0 {
		t.Errorf("seg2 missing (executor should be online): %+v", e.Seg2)
	}
	if e.Total.LatencyMS == nil || *e.Total.LatencyMS != *st.Seg1.LatencyMS+*e.Seg2.LatencyMS {
		t.Errorf("total should equal seg1+seg2: %+v", e.Total)
	}
}

// 端到端:多执行方,status 返回所有已注册执行方,且各自段的时延互不影响。
func TestEndToEnd_Status_MultiExecutor(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()

	for _, wid := range []string{"site-a", "site-b"} {
		if _, err := NewRelayBackend(map[string]interface{}{
			"url": wsURL, "token": "tok-exe", "executor_id": wid, "watch_dir": ".", "executor": true,
		}); err != nil {
			t.Fatalf("executor backend %s: %v", wid, err)
		}
	}

	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "site-a", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	st, err := rb.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Executors) != 2 {
		t.Fatalf("executors: got %d, want 2: %+v", len(st.Executors), st.Executors)
	}
	for _, e := range st.Executors {
		if e.Seg2.LatencyMS == nil || *e.Seg2.LatencyMS < 0 {
			t.Errorf("executor %q seg2 missing: %+v", e.ExecutorID, e.Seg2)
		}
		if e.Total.LatencyMS == nil {
			t.Errorf("executor %q total missing: %+v", e.ExecutorID, e.Total)
		}
	}
}

// 端到端:全部执行方离线时 status 段1 给出、执行方列表为空,仍成功(中转在线、远端掉线)。
func TestEndToEnd_Status_ExecutorOffline(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()
	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	st, err := rb.Status(ctx)
	if err != nil {
		t.Fatalf("status should succeed with all executors offline: %v", err)
	}
	if st.Seg1.LatencyMS == nil {
		t.Errorf("seg1 should be present (transit online): %+v", st.Seg1)
	}
	if len(st.Executors) != 0 {
		t.Errorf("executors should be empty when all offline, got %+v", st.Executors)
	}
}

func TestMsysToWindowsPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/d/Group_Projects/databus_backend", `D:\Group_Projects\databus_backend`},
		{"/d/foo", `D:\foo`},
		{"/d", `D:\`},                              // 仅盘符根 -> D:\
		{"/data/relay", `/data/relay`},             // 多字符盘名,不转换
		{`D:\already\native`, `D:\already\native`}, // 已是原生,原样
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, c := range cases {
		if got := msysToWindowsPath(c.in); got != c.want {
			t.Errorf("msysToWindowsPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func testRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v (output: %s)", strings.Join(args, " "), err, string(out))
	}
}

// 端到端:通过 ExecStreamWithGitSync 在执行命令前完成 Git 增量补丁同步与应用。
func TestEndToEnd_ExecWithGitSync(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 准备本地与远端 Git 仓库目录
	localDir := t.TempDir()
	remoteDir := t.TempDir()

	testRunGit(t, localDir, "init")
	testRunGit(t, localDir, "config", "user.name", "TestUser")
	testRunGit(t, localDir, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(localDir, "main.go"), []byte("package main // initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, localDir, "add", "main.go")
	testRunGit(t, localDir, "commit", "-m", "init commit")

	// 克隆到远端工作区，保持基准 commit 完全一致
	testRunGit(t, remoteDir, "clone", localDir, ".")
	testRunGit(t, remoteDir, "config", "user.name", "TestUser")
	testRunGit(t, remoteDir, "config", "user.email", "test@test.com")

	// 2. 启动中转服务器
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	ctx := context.Background()

	// 3. 启动远端执行方 (executor_dir 指向 remoteDir)
	if _, err := NewRelayBackend(map[string]interface{}{
		"url":          wsURL,
		"token":        "tok-exe",
		"executor_id":  "test",
		"watch_dir":    ".",
		"executor_dir": remoteDir,
		"executor":     true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 在本地修改代码并产生未跟踪新文件
	if err := os.WriteFile(filepath.Join(localDir, "main.go"), []byte("package main // modified locally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "untracked.txt"), []byte("untracked file content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 5. 本地提取 diff
	diffPayload, err := git.CreateDiffPayload(localDir, false)
	if err != nil {
		t.Fatalf("create diff payload: %v", err)
	}

	// 6. 启动请求方后端
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "test",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	rb, ok := reqBackend.(bk.GitSyncExecBackend)
	if !ok {
		t.Fatalf("requester backend does not implement GitSyncExecBackend")
	}

	// 7. 发起带增量同步的远程命令执行
	var output strings.Builder
	exit, err := rb.ExecStreamWithGitSync(ctx, "", "cat main.go && cat untracked.txt", "", 10, &bk.GitSyncPayload{
		BaseCommit:       diffPayload.BaseCommit,
		Patch:            diffPayload.Patch,
		UntrackedArchive: diffPayload.UntrackedArchive,
		Force:            diffPayload.Force,
	}, func(c bk.ExecChunk) {
		output.WriteString(c.Data)
	})

	if err != nil {
		t.Fatalf("exec with git sync failed: %v", err)
	}
	if exit != 0 {
		t.Fatalf("unexpected exit code: %d, output: %s", exit, output.String())
	}

	outStr := output.String()
	if !strings.Contains(outStr, "[git-sync] applied incremental diff") {
		t.Errorf("output missing git-sync confirmation: %s", outStr)
	}
	if !strings.Contains(outStr, "package main // modified locally") {
		t.Errorf("remote command did not see modified main.go: %s", outStr)
	}
	if !strings.Contains(outStr, "untracked file content") {
		t.Errorf("remote command did not see untracked.txt: %s", outStr)
	}

	// 8. 检查远端工作区文件真实状态
	remoteContent, err := os.ReadFile(filepath.Join(remoteDir, "main.go"))
	if err != nil {
		t.Fatalf("read remote main.go: %v", err)
	}
	if string(remoteContent) != "package main // modified locally\n" {
		t.Errorf("remote file content mismatch: %s", string(remoteContent))
	}
}

// 端到端:测试 TaskSupervisor 的后台脱机执行 (-d)、任务列表查询 (ps)、日志跟随 (logs -f) 与停止 (stop)
func TestEndToEnd_TaskSupervisor(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 任务目录隔离到临时目录,不碰真实 ~/.relay。
	withSupervisorBaseDir(t)
	defer Cleanup()

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	ctx := context.Background()

	// 1. 启动远端执行方
	if _, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-1",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 2. 启动请求方
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "node-1",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	tb, ok := reqBackend.(bk.TaskSupervisorBackend)
	if !ok {
		t.Fatalf("requester does not implement TaskSupervisorBackend")
	}

	gb, ok := reqBackend.(bk.GitSyncExecBackend)
	if !ok {
		t.Fatalf("requester does not implement GitSyncExecBackend")
	}

	// 3. 后台脱机执行任务 (detach = true, 带 ports)。绑定面用非回环值,顺带钉住 R21:
	// 记录必须原样带走 BindHost,不能退化成 String() 的展示形态。
	taskPorts := []portfwd.Spec{{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080}}
	var taskIDOut strings.Builder
	start := time.Now()
	exit, err := gb.ExecStreamWithPorts(ctx, "node-1", "printf 'task_step_1\n'; sleep 0.3; printf 'task_step_2\n'; sleep 5", "", 10, nil, true, taskPorts, "", func(c bk.ExecChunk) {
		taskIDOut.WriteString(c.Data)
	})
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("exec detach failed: %v", err)
	}
	if exit != 0 {
		t.Fatalf("detach exit code: %d", exit)
	}
	if dur > 1*time.Second {
		t.Errorf("detach command took too long: %s (expected non-blocking)", dur)
	}

	taskIDRaw := strings.TrimSpace(taskIDOut.String())
	// backend 层 detach 输出现在以 ForwardTaskIDMarker 为前缀回写任务 ID,供调用方精确识别。
	taskID := strings.TrimSpace(strings.TrimPrefix(taskIDRaw, protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(taskID, "t-") {
		t.Fatalf("invalid taskID output: %q", taskIDRaw)
	}

	// 3b. 相同命令+端口再次 detach 必须复用同一任务(幂等),不能产生第二个实例。
	var reuseOut strings.Builder
	exit, err = gb.ExecStreamWithPorts(ctx, "node-1", "printf 'task_step_1\n'; sleep 0.3; printf 'task_step_2\n'; sleep 5", "", 10, nil, true, taskPorts, "", func(c bk.ExecChunk) {
		reuseOut.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("second exec detach failed: %v", err)
	}
	if exit != 0 {
		t.Fatalf("second detach exit code: %d", exit)
	}
	reuseID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reuseOut.String()), protocol.ForwardTaskIDMarker))
	if reuseID != taskID {
		t.Fatalf("second detach created task %s, want reuse of %s", reuseID, taskID)
	}

	// 4. 查询任务列表 (ps)
	tasks, err := tb.TaskList(ctx, "node-1", false)
	if err != nil {
		t.Fatalf("task list: %v", err)
	}
	var found bool
	for _, tk := range tasks {
		if tk.ID == taskID {
			found = true
			if tk.Status != "running" {
				t.Errorf("expected running status, got: %s", tk.Status)
			}
			if !reflect.DeepEqual(tk.Ports, taskPorts) {
				t.Errorf("expected ports %+v, got %+v", taskPorts, tk.Ports)
			}
			break
		}
	}
	if !found {
		t.Fatalf("task %s not found in active task list", taskID)
	}

	// 5. 日志流式抓取 (logs -f)
	logCtx, cancelLog := context.WithTimeout(ctx, 2*time.Second)
	defer cancelLog()

	var logs strings.Builder
	_ = tb.TaskLogs(logCtx, "node-1", taskID, true, 10, func(c bk.ExecChunk) {
		logs.WriteString(c.Data)
		if strings.Contains(logs.String(), "task_step_2") {
			cancelLog()
		}
	})

	loggedStr := logs.String()
	if !strings.Contains(loggedStr, "task_step_1") {
		t.Errorf("logs missing step 1, got: %q", loggedStr)
	}

	// 6. 停止任务 (stop)
	if err := tb.TaskStop(ctx, "node-1", taskID); err != nil {
		t.Fatalf("task stop: %v", err)
	}

	// 7. 再次查询 (ps -a) 验证状态变为 stopped
	allTasks, err := tb.TaskList(ctx, "node-1", true)
	if err != nil {
		t.Fatalf("task list all: %v", err)
	}
	var stoppedFound bool
	for _, tk := range allTasks {
		if tk.ID == taskID {
			stoppedFound = true
			if tk.Status != "stopped" {
				t.Errorf("expected stopped status, got: %s", tk.Status)
			}
			break
		}
	}
	if !stoppedFound {
		t.Fatalf("task %s not found in all tasks list", taskID)
	}
}

// 端到端:测试任务重启 (relay restart)。请求经中转按 ExecutorID 路由到指定执行方,
// 由执行方在原 id 上重跑记录中的命令;对不存在的任务 id、错误的执行方与离线执行方
// 都必须在超时内返回可读错误,而不是挂起。
func TestEndToEnd_TaskRestart(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 任务目录隔离到临时目录,不碰真实 ~/.relay。
	baseDir := withSupervisorBaseDir(t)
	defer Cleanup()

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	ctx := context.Background()

	// 1. 两个执行方在线:node-1 持有任务,node-2 用于验证请求不会误投。
	for _, id := range []string{"node-1", "node-2"} {
		if _, err := NewRelayBackend(map[string]interface{}{
			"url":         wsURL,
			"token":       "tok-exe",
			"executor_id": id,
			"watch_dir":   ".",
			"executor":    true,
		}); err != nil {
			t.Fatalf("executor %s backend: %v", id, err)
		}
	}

	// 2. 请求方
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "node-1",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	tb, ok := reqBackend.(bk.TaskSupervisorBackend)
	if !ok {
		t.Fatalf("requester does not implement TaskSupervisorBackend")
	}
	gb, ok := reqBackend.(bk.GitSyncExecBackend)
	if !ok {
		t.Fatalf("requester does not implement GitSyncExecBackend")
	}

	// 3. node-2 的任务清单查询要能正常应答。
	if _, err := tb.TaskList(ctx, "node-2", true); err != nil {
		t.Fatalf("node-2 task list: %v", err)
	}

	// 4. 在 node-1 上后台起一个长命任务
	var taskIDOut strings.Builder
	exit, err := gb.ExecStreamWithPorts(ctx, "node-1", "printf 'restart_me\n'; sleep 30", "", 10, nil, true, nil, "", func(c bk.ExecChunk) {
		taskIDOut.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("exec detach failed: %v", err)
	}
	if exit != 0 {
		t.Fatalf("detach exit code: %d", exit)
	}
	taskID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(taskIDOut.String()), protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(taskID, "t-") {
		t.Fatalf("invalid taskID output: %q", taskIDOut.String())
	}
	// defer 晚于函数的客户端关闭注册,按 LIFO 先于 client.CloseAll() 执行。
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tb.TaskStop(stopCtx, "node-1", taskID)
	}()

	// 4. 重启运行中的任务:收到成功应答,任务仍以原 id 处于运行态
	if err := tb.TaskRestart(ctx, "node-1", taskID); err != nil {
		t.Fatalf("task restart: %v", err)
	}
	tasks, err := tb.TaskList(ctx, "node-1", false)
	if err != nil {
		t.Fatalf("task list after restart: %v", err)
	}
	var running bool
	for _, tk := range tasks {
		if tk.ID == taskID {
			running = true
			if tk.Status != "running" {
				t.Errorf("expected task running after restart, got: %s", tk.Status)
			}
			break
		}
	}
	if !running {
		t.Fatalf("task %s not running after restart (identity must be preserved)", taskID)
	}

	// 5. 不存在的 task id:失败应答且错误可读,不挂起
	missingCtx, cancelMissing := context.WithTimeout(ctx, 3*time.Second)
	defer cancelMissing()
	err = tb.TaskRestart(missingCtx, "node-1", "t-does-not-exist")
	if err == nil {
		t.Fatalf("expected error for unknown task id")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown task id should fail fast, got timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected readable 'not found' error, got: %v", err)
	}

	// 6. 路由:地址指向 node-2 时不得投到持有该任务的 node-1。supervisor 按任务目录每进程唯一,
	// 两台进程内执行方因此共用同一份;把 node-2 指向另一个任务目录,复现真实部署里「任务只存在于
	// 创建它的那个执行方」这一事实。
	otherCtx, cancelOther := context.WithTimeout(ctx, 3*time.Second)
	defer cancelOther()
	supervisorBaseDir = t.TempDir()
	err = tb.TaskRestart(otherCtx, "node-2", taskID)
	supervisorBaseDir = baseDir
	if err == nil {
		t.Fatalf("request addressed to node-2 must not reach node-1")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("misrouted request should fail fast, got timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected node-2's 'not found' error, got: %v", err)
	}

	// 7. 执行方离线:在超时内拿到错误而不是一直等待
	offlineCtx, cancelOffline := context.WithTimeout(ctx, 3*time.Second)
	defer cancelOffline()
	err = tb.TaskRestart(offlineCtx, "node-ghost", taskID)
	if err == nil {
		t.Fatalf("expected error for offline executor")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("offline executor should fail fast, got timeout: %v", err)
	}
}

// 端到端:通过 TunnelOpen 实现端口映射直通远端内部服务,并验证白名单防护。
func TestEndToEnd_PortForward(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 模拟远端本地 TCP Echo 服务
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	defer ln.Close()
	echoPort := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	// 2. 启动具备隧道功能的中转服务器
	watchDir := t.TempDir()
	ts, wsURL := newTestHubWithTunnel(t, watchDir)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 3. 执行方(node-1)接入且 network_allow 为空(默认拒绝常规出网隧道)
	_, err = NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-1",
		"watch_dir":   ".",
		"executor":    true,
		// network_allow 为空: fail-closed，阻止常规出网隧道
	})
	if err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 请求方接入
	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "req-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	// 5. 开启开发用端口转发隧道(TunnelOpenPortForward)，跳过 network_allow 白名单
	stream, err := rb.TunnelOpenPortForward(ctx, "node-1", "127.0.0.1", uint16(echoPort))
	if err != nil {
		t.Fatalf("port-forward tunnel open: %v", err)
	}

	payload := []byte("hello-relay-port-forward\n")
	if err := stream.Send(payload); err != nil {
		t.Fatalf("stream send: %v", err)
	}

	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream recv: %v", err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("expected %q, got %q", string(payload), string(reply))
	}
	stream.Close()

	// 6. 验证常规出网隧道(TunnelOpen)仍然受白名单限制(此时 network_allow 为空应被拦截)
	_, err = rb.TunnelOpen(ctx, "node-1", "127.0.0.1", uint16(echoPort))
	if err == nil {
		t.Fatalf("expected network_allow reject error for normal TunnelOpen, got nil")
	}

	// 7. 验证不存在的目标节点错误
	_, err = rb.TunnelOpenPortForward(ctx, "non-existent-node", "127.0.0.1", uint16(echoPort))
	if err == nil {
		t.Fatalf("expected error for non-existent node, got nil")
	}
}

// 端到端:通过 GitStatus 和 GitUndo 实现远端工作区影子快照查询与一键恢复。
func TestEndToEnd_GitUndoAndStatus(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 准备本地与远端 Git 仓库并保持基准提交对齐
	localDir := t.TempDir()
	remoteDir := t.TempDir()

	testRunGit(t, localDir, "init")
	testRunGit(t, localDir, "config", "user.name", "TestUser")
	testRunGit(t, localDir, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(localDir, "app.go"), []byte("package main // initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, localDir, "add", "app.go")
	testRunGit(t, localDir, "commit", "-m", "initial commit")

	testRunGit(t, remoteDir, "clone", localDir, ".")
	testRunGit(t, remoteDir, "config", "user.name", "TestUser")
	testRunGit(t, remoteDir, "config", "user.email", "test@test.com")

	// 2. 启动中转服务器
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 3. 执行方(node-git)接入，工作目录指向 remoteDir
	_, err := NewRelayBackend(map[string]interface{}{
		"url":          wsURL,
		"token":        "tok-exe",
		"executor_id":  "node-git",
		"watch_dir":    ".",
		"executor":     true,
		"executor_dir": remoteDir,
	})
	if err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 请求方接入
	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "req-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	// 5. 初始查询远端 Git 状态
	statusResp, err := rb.GitStatus(ctx, "node-git", "")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !statusResp.IsGitRepo {
		t.Fatalf("expected remote to be git repo")
	}
	if statusResp.BaseCommit == "" {
		t.Fatalf("expected remote to have base commit")
	}
	if len(statusResp.Snapshots) != 0 {
		t.Fatalf("expected 0 snapshots initially, got %d", len(statusResp.Snapshots))
	}

	// 6. 远端在本地开发过程中产生修改
	if err := os.WriteFile(filepath.Join(remoteDir, "app.go"), []byte("package main // important remote edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "scratch.txt"), []byte("remote scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 7. 发起一次 ExecStreamWithGitSync 模拟从请求方执行命令覆盖同步
	// 本地做出修改
	if err := os.WriteFile(filepath.Join(localDir, "app.go"), []byte("package main // local overwritten version\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	localPayload, err := git.CreateDiffPayload(localDir, false)
	if err != nil {
		t.Fatalf("create local diff: %v", err)
	}

	var outBuf bytes.Buffer
	_, err = rb.ExecStreamWithGitSync(ctx, "node-git", "echo sync_done", remoteDir, 5, &bk.GitSyncPayload{
		BaseCommit:       localPayload.BaseCommit,
		Patch:            localPayload.Patch,
		UntrackedArchive: localPayload.UntrackedArchive,
		Force:            localPayload.Force,
	}, func(c bk.ExecChunk) {
		outBuf.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("exec with git sync: %v", err)
	}

	// 验证远端此时已经被本地覆盖
	remoteContent, err := os.ReadFile(filepath.Join(remoteDir, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remoteContent), "local overwritten version") {
		t.Fatalf("expected local overwritten version, got %s", string(remoteContent))
	}

	// 8. 查询快照列表，断言自动创建了影子快照
	statusResp2, err := rb.GitStatus(ctx, "node-git", "")
	if err != nil {
		t.Fatalf("git status 2: %v", err)
	}
	if len(statusResp2.Snapshots) < 1 {
		t.Fatalf("expected at least 1 snapshot after sync on dirty remote, got %d", len(statusResp2.Snapshots))
	}

	// 9. 执行 GitUndo 一键还原
	undoResp, err := rb.GitUndo(ctx, "node-git", "", "")
	if err != nil {
		t.Fatalf("git undo: %v", err)
	}
	if !undoResp.OK {
		t.Fatalf("git undo response not ok: %s", undoResp.Error)
	}

	// 10. 验证远端文件成功恢复为覆盖前的状态 (important remote edit)
	restoredContent, err := os.ReadFile(filepath.Join(remoteDir, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restoredContent), "important remote edit") {
		t.Errorf("expected restored remote edit, got: %s", string(restoredContent))
	}

	// 验证未跟踪的草稿文件也被恢复
	scratchContent, err := os.ReadFile(filepath.Join(remoteDir, "scratch.txt"))
	if err != nil {
		t.Errorf("expected scratch.txt to be restored, got error: %v", err)
	} else if string(scratchContent) != "remote scratch\n" {
		t.Errorf("scratch.txt = %q, want 'remote scratch\\n'", string(scratchContent))
	}
}

// readSeedMetaStartTime 读取磁盘元数据里的最近启动时间,用于验证重放的 once 守卫。
func readSeedMetaStartTime(t *testing.T, baseDir, id string) time.Time {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(baseDir, id, "meta.json"))
	if err != nil {
		t.Fatalf("read meta %s: %v", id, err)
	}
	var m struct {
		StartTime time.Time `json:"start_time"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal meta %s: %v", id, err)
	}
	return m.StartTime
}

// TestEndToEnd_ExecutorStartupReplaysAlwaysTasks 覆盖执行方进程启动时的重放(R6、KTD4、KTD10):
// 真实执行方(经 in-process hub 注册)启动路径触发一次重放后,上一次进程留下的 always 任务
// (含优雅停机写成 stopped 的)自动回到运行态,no 任务保持停止;同进程内第二次触发是 no-op,
// 不会杀起重放。
func TestEndToEnd_ExecutorStartupReplaysAlwaysTasks(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 任务目录隔离到临时目录,不碰真实 ~/.relay。
	baseDir := withSupervisorBaseDir(t)
	defer Cleanup()

	// 上一次执行方进程留下的两个任务,随后优雅停机(只写 stopped,策略保留)。
	seed, err := supervisor.New(baseDir)
	if err != nil {
		t.Fatalf("seed supervisor: %v", err)
	}
	alwaysTask, err := seed.StartTaskWithPolicy("sleep 30", "", nil, supervisor.RestartAlways, nil)
	if err != nil {
		t.Fatalf("seed always task: %v", err)
	}
	noTask, err := seed.StartTaskWithPolicy("sleep 30", "", nil, supervisor.RestartNo, nil)
	if err != nil {
		t.Fatalf("seed no task: %v", err)
	}
	alwaysID, noID := alwaysTask.Meta().ID, noTask.Meta().ID
	seed.StopAll()

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	// 真实执行方启动:以 executor 角色注册到 in-process hub(与 relay watch 同一路径)。
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "node-1", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 执行方进程启动路径显式触发一次重放(KTD4)。
	ReplayAlwaysTasks()

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "node-1", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	tb, ok := reqBackend.(bk.TaskSupervisorBackend)
	if !ok {
		t.Fatalf("requester does not implement TaskSupervisorBackend")
	}

	ctx := context.Background()
	tasks, err := tb.TaskList(ctx, "node-1", true)
	if err != nil {
		t.Fatalf("task list: %v", err)
	}
	byID := map[string]bk.TaskInfo{}
	for _, tk := range tasks {
		byID[tk.ID] = tk
	}

	gotAlways, ok := byID[alwaysID]
	if !ok {
		t.Fatalf("replayed always task %s missing from task list", alwaysID)
	}
	if gotAlways.Status != string(supervisor.StatusRunning) {
		t.Errorf("always task not replayed: want running, got %s", gotAlways.Status)
	}
	if gotAlways.Restart != supervisor.RestartAlways {
		t.Errorf("always task policy lost over the wire: got %q", gotAlways.Restart)
	}
	if gotAlways.CreatedAt == 0 {
		t.Errorf("task briefing should carry the first-created time")
	}

	gotNo, ok := byID[noID]
	if !ok {
		t.Fatalf("no task %s missing from task list", noID)
	}
	if gotNo.Status == string(supervisor.StatusRunning) {
		t.Errorf("no task must not be replayed, got %s", gotNo.Status)
	}
	if gotNo.Restart != supervisor.RestartNo {
		t.Errorf("no task policy: want %q, got %q", supervisor.RestartNo, gotNo.Restart)
	}

	// 同进程内再次触发是 no-op:任务不会被杀起重放。
	startBefore := readSeedMetaStartTime(t, baseDir, alwaysID)
	ReplayAlwaysTasks()
	startAfter := readSeedMetaStartTime(t, baseDir, alwaysID)
	if !startAfter.Equal(startBefore) {
		t.Errorf("replay guard failed: second trigger relaunched the task (%s -> %s)", startBefore, startAfter)
	}
}

// TestEndToEnd_ExplicitStopClearsRestartPolicy 覆盖 relay stop 清除策略(R7/KD5):带 always 的
// detach 任务经中转真正创建并携带策略,stop 后元数据策略变 no,执行方下次启动不重放它。
func TestEndToEnd_ExplicitStopClearsRestartPolicy(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	baseDir := withSupervisorBaseDir(t)
	defer Cleanup()

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "node-1", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "node-1", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	gb, ok := reqBackend.(bk.GitSyncExecBackend)
	if !ok {
		t.Fatalf("requester does not implement GitSyncExecBackend")
	}

	ctx := context.Background()
	var out strings.Builder
	exit, err := gb.ExecStreamWithPorts(ctx, "node-1", "sleep 30", "", 10, nil, true, nil, supervisor.RestartAlways, func(c bk.ExecChunk) {
		out.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("exec detach: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exec detach exit: %d", exit)
	}
	taskID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.String()), protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(taskID, "t-") {
		t.Fatalf("invalid taskID: %q", out.String())
	}

	tb := reqBackend.(bk.TaskSupervisorBackend)
	find := func() bk.TaskInfo {
		t.Helper()
		list, err := tb.TaskList(ctx, "node-1", true)
		if err != nil {
			t.Fatalf("task list: %v", err)
		}
		for _, tk := range list {
			if tk.ID == taskID {
				return tk
			}
		}
		t.Fatalf("task %s not found in task list", taskID)
		return bk.TaskInfo{}
	}

	if got := find().Restart; got != supervisor.RestartAlways {
		t.Fatalf("policy not carried to executor metadata: got %q", got)
	}

	if err := tb.TaskStop(ctx, "node-1", taskID); err != nil {
		t.Fatalf("task stop: %v", err)
	}

	stopped := find()
	if stopped.Restart != supervisor.RestartNo {
		t.Errorf("explicit stop must clear the policy: got %q", stopped.Restart)
	}
	if stopped.Status == string(supervisor.StatusRunning) {
		t.Errorf("task should be stopped, got %s", stopped.Status)
	}

	// 执行方下次启动不重放它。
	sup, err := supervisor.New(baseDir)
	if err != nil {
		t.Fatalf("reload supervisor: %v", err)
	}
	if results := sup.ReplayAlways(); len(results) != 0 {
		t.Fatalf("unexpected replay results: %+v", results)
	}
	reloaded, err := sup.GetTask(taskID)
	if err != nil {
		t.Fatalf("get reloaded task: %v", err)
	}
	if reloaded.Meta().Status == supervisor.StatusRunning {
		t.Errorf("explicitly stopped task must not be replayed on executor startup")
	}
}

// TestEndToEnd_PortSpecRoundTripIsLossless 覆盖 U6a 的完整链路(R21/KTD9):
// CLI 侧解析 → 执行请求(wire) → 执行方任务记录(meta.json) → 任务简报(wire) → CLI 侧表格。
// 绑定主机必须全程无损 —— 旧形态只留 String() 的展示字符串,-p 0.0.0.0:8080 会与 -p 8080 落成
// 同一个 "8080",此后重登记只能悄悄收窄回环。
func TestEndToEnd_PortSpecRoundTripIsLossless(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	defer client.CloseAll()

	// 任务目录隔离到临时目录,不碰真实 ~/.relay。
	baseDir := withSupervisorBaseDir(t)
	defer Cleanup()

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()

	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "node-1", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "node-1", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	// CLI 与执行方同源的解析:五种形态解析后上行,不只是把字符串转个手。
	var specs []portfwd.Spec
	for _, raw := range []string{"8080", "8080:9000", "0.0.0.0:8080", "0.0.0.0:8080:9000", "8080:db.local:5432"} {
		spec, err := portfwd.Parse(raw, portfwd.DefaultBind)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		specs = append(specs, *spec)
	}

	ctx := context.Background()
	gb, ok := reqBackend.(bk.GitSyncExecBackend)
	if !ok {
		t.Fatalf("requester does not implement GitSyncExecBackend")
	}
	var out strings.Builder
	exit, err := gb.ExecStreamWithPorts(ctx, "node-1", "sleep 30", "", 10, nil, true, specs, "", func(c bk.ExecChunk) {
		out.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("exec detach: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exec detach exit: %d", exit)
	}
	taskID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.String()), protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(taskID, "t-") {
		t.Fatalf("invalid taskID: %q", out.String())
	}

	// 1) 记录侧:执行方磁盘上的 meta.json 逐字段无损,且确实带上了绑定面。
	metaBytes, err := os.ReadFile(filepath.Join(baseDir, taskID, "meta.json"))
	if err != nil {
		t.Fatalf("read task meta: %v", err)
	}
	var onDisk struct {
		Ports []portfwd.Spec `json:"ports"`
	}
	if err := json.Unmarshal(metaBytes, &onDisk); err != nil {
		t.Fatalf("unmarshal task meta: %v", err)
	}
	if !reflect.DeepEqual(onDisk.Ports, specs) {
		t.Errorf("recorded ports = %+v, want %+v", onDisk.Ports, specs)
	}
	var rawPorts struct {
		Ports []map[string]json.RawMessage `json:"ports"`
	}
	if err := json.Unmarshal(metaBytes, &rawPorts); err != nil {
		t.Fatalf("unmarshal raw meta: %v", err)
	}
	for i, p := range rawPorts.Ports {
		if _, ok := p["bind"]; !ok {
			t.Errorf("recorded ports[%d] has no bind field: %v (record degraded to display strings)", i, p)
		}
	}

	// 2) 简报侧:经 wire 回到 CLI 的结果同样无损(relay ps 读到的是这一份)。
	tb, ok := reqBackend.(bk.TaskSupervisorBackend)
	if !ok {
		t.Fatalf("requester does not implement TaskSupervisorBackend")
	}
	tasks, err := tb.TaskList(ctx, "node-1", true)
	if err != nil {
		t.Fatalf("task list: %v", err)
	}
	var briefed bk.TaskInfo
	var found bool
	for _, tk := range tasks {
		if tk.ID == taskID {
			briefed, found = tk, true
			break
		}
	}
	if !found {
		t.Fatalf("task %s not found in task list", taskID)
	}
	if !reflect.DeepEqual(briefed.Ports, specs) {
		t.Errorf("briefed ports = %+v, want %+v", briefed.Ports, specs)
	}
	if briefed.Ports[2].BindHost != "0.0.0.0" {
		t.Errorf("briefed -p 0.0.0.0:8080 bind = %q, want 0.0.0.0 (not narrowed to loopback)", briefed.Ports[2].BindHost)
	}
}
