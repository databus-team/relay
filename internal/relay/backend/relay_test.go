package backend

import (
	"context"
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

// end-to-end: 请求方 PushJob 直达执行方,执行方落盘并跑本地 job,输出流回。
func TestEndToEnd_PushJob(t *testing.T) {
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
