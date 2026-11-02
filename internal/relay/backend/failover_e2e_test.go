package backend

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bk "github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/server"
)

func newTestHubWithToken(t testing.TB, watchDir string, token string) (*httptest.Server, string) {
	t.Helper()
	srv, err := server.New(server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{token},
		},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
}

func TestEndToEnd_MultiHubFailoverAndReRegister(t *testing.T) {
	SetExecutorRole(true)
	defer SetExecutorRole(false)
	withSupervisorBaseDir(t)

	hub1Dir := t.TempDir()
	hub2Dir := t.TempDir()

	hub1, hub1URL := newTestHubWithToken(t, hub1Dir, "tok-hub1")
	hub2, hub2URL := newTestHubWithToken(t, hub2Dir, "tok-hub2")
	defer hub2.Close()

	agentParams := map[string]interface{}{
		"hubs": []interface{}{
			map[string]interface{}{
				"url":   hub1URL,
				"token": "tok-hub1",
			},
			map[string]interface{}{
				"url":   hub2URL,
				"token": "tok-hub2",
			},
		},
		"executor":    true,
		"executor_id": "failover-worker",
		"watch_dir":   "test",
	}

	bRaw, err := NewRelayBackend(agentParams)
	if err != nil {
		t.Fatalf("create agent backend: %v", err)
	}
	agentBackend := bRaw.(*RelayBackend)

	if agentBackend.ActiveURL() != hub1URL {
		t.Fatalf("initial active URL = %s, want %s (hub1)", agentBackend.ActiveURL(), hub1URL)
	}

	// 模拟 Hub1 宕机
	hub1.Close()
	agentBackend.client.Disconnect()

	// 等待漂移到 Hub2
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if agentBackend.ActiveURL() == hub2URL {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if agentBackend.ActiveURL() != hub2URL {
		t.Fatalf("agent failed to failover to hub2; activeURL=%s", agentBackend.ActiveURL())
	}

	// 此时由控制端连接 Hub2，向 failover-worker 发送 ExecStream 请求，验证自注册与路由恢复
	reqRaw, err := NewRelayBackend(map[string]interface{}{
		"url":         hub2URL,
		"token":       "tok-hub2",
		"executor_id": "requester",
		"watch_dir":   "test",
	})
	if err != nil {
		t.Fatalf("create requester backend: %v", err)
	}
	reqBackend := reqRaw.(bk.ExecStreamBackend)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out []string
	exitCode, err := reqBackend.ExecStream(ctx, "failover-worker", "echo failover-success", "", 10, func(chunk bk.ExecChunk) {
		out = append(out, chunk.Data)
	})

	if err != nil {
		t.Fatalf("exec on failed-over worker: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exec exit code = %d, want 0", exitCode)
	}
	combined := strings.Join(out, "")
	if !strings.Contains(combined, "failover-success") {
		t.Errorf("expected output to contain 'failover-success', got: %s", combined)
	}
}
