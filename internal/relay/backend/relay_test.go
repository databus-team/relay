package backend

import (
	"bytes"
	"context"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bk "github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/server"
)

func newTestHub(t *testing.T, watchDir string) (*httptest.Server, string) {
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 请求方:只发起 exec,不做 executor
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
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
	exit, err := rb.ExecStream(ctx, "printf 'one\\ntwo\\nthree\\n'", "", 10, func(c bk.ExecChunk) {
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "executor": true,
		"config_path": configPath,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 请求方:走 config-sync 流程
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	cs, ok := reqBackend.(bk.ConfigSyncCapable)
	if !ok {
		t.Fatalf("requester backend does not implement ConfigSyncCapable")
	}

	payload := []byte("name: relay\nversion: 2\nbackend:\n  type: relay\n")
	exit, err := cs.ConfigSync(ctx, payload)
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

// end-to-end: 请求方 PushJob 直达执行方,执行方落盘并跑本地 job,推送流回。
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	// (handler 由 watcher 注入,直接构造再取下执行方 backend 对象比较麻烦;改为经 client 单测覆盖。)

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	pj, ok := reqBackend.(bk.PushJobSender)
	if !ok {
		t.Fatalf("requester backend does not implement PushJobSender")
	}

	var streamed []string
	exit, err := pj.PushJob(ctx, "sub/f.txt", []byte("hello from push\n"), func(c bk.ExecChunk) {
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	tn, ok := reqBackend.(bk.PushNoJobsSender)
	if !ok {
		t.Fatalf("requester backend does not implement PushNoJobsSender")
	}

	dest := filepath.Join(execRoot, "sub", "relay.new")
	exit, err := tn.PushNoJobs(ctx, dest, []byte("deploy blob\n"))
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "executor": true, "executor_dir": execRoot,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	reqBackend, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
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

	exit, err := pj.PushJob(ctx, "big.bin", blob, func(bk.ExecChunk) {})
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
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	pj := reqBackend.(bk.PushJobSender)

	var streamed []string
	exit, err := pj.PushJob(ctx, "fallback.txt", []byte("staged\n"), func(c bk.ExecChunk) {
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
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}

	rb := reqBackend.(bk.ExecStreamBackend)
	if _, err := rb.ExecStream(context.Background(), "echo x", "", 3, nil); err == nil {
		t.Error("expected error when no executor, got nil")
	} else if !strings.Contains(err.Error(), "no executor") {
		t.Errorf("unexpected error: %v", err)
	}
}

// 端到端:执行方在线时,relay 后端 status 返回 三段 + 台账(中转+执行方)。
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
		"url": wsURL, "token": "tok-exe", "watch_id": "test", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
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
	if st.Seg2.LatencyMS == nil || *st.Seg2.LatencyMS < 0 {
		t.Errorf("seg2 missing (executor should be online): %+v", st.Seg2)
	}
	if st.Total.LatencyMS == nil || *st.Total.LatencyMS != *st.Seg1.LatencyMS+*st.Seg2.LatencyMS {
		t.Errorf("total should equal seg1+seg2: %+v", st.Total)
	}

	var hasTransit, hasExecutor bool
	for _, n := range st.Nodes {
		switch n.Role {
		case "transit":
			hasTransit = true
		case "executor":
			if n.WatchID == "test" {
				hasExecutor = true
			}
		}
	}
	if !hasTransit || !hasExecutor {
		t.Errorf("ledger should include transit and executor: %+v", st.Nodes)
	}
}

// 端到端:执行方离线时 status 段1 给出、段2 标不可用,仍成功(中转在线、远端掉线)。
func TestEndToEnd_Status_ExecutorOffline(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	defer ts.Close()
	defer client.CloseAll()

	ctx := context.Background()
	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "watch_id": "test", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*RelayBackend)

	st, err := rb.Status(ctx)
	if err != nil {
		t.Fatalf("status should succeed with executor offline: %v", err)
	}
	if st.Seg1.LatencyMS == nil {
		t.Errorf("seg1 should be present (transit online): %+v", st.Seg1)
	}
	if st.Seg2.LatencyMS != nil {
		t.Errorf("seg2.latency_ms should be nil when executor offline, got %d", *st.Seg2.LatencyMS)
	}
	if st.Seg2.Unavailable == "" {
		t.Error("seg2 should mark executor offline")
	}
	if st.Total.LatencyMS != nil {
		t.Errorf("total should be unavailable when seg2 unavailable, got %d", *st.Total.LatencyMS)
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
