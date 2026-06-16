package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/server"
)

func setupTestServer(t *testing.T, watchDir string) (*httptest.Server, string) {
	t.Helper()

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: []string{"test-token"},
		},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	ts := httptest.NewServer(srv)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	return ts, wsURL
}

func connectTestClient(t *testing.T, wsURL string) *client.Client {
	t.Helper()
	ctx := context.Background()
	c, err := client.New(wsURL, "test-token", "test-watch")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	c.SetReconnectEnabled(false)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

func TestIntegration_ListDir(t *testing.T) {
	watchDir := t.TempDir()

	for _, name := range []string{"a.txt", "b.txt", "c.bin"} {
		os.WriteFile(filepath.Join(watchDir, name), []byte("hello"), 0644)
	}

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	entries, err := c.List(ctx, ".")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"a.txt", "b.txt", "c.bin"} {
		if !names[want] {
			t.Errorf("missing entry: %s", want)
		}
	}
}

func TestIntegration_PushPull(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	content := []byte("hello world from relay test")
	if err := c.Push(ctx, "test-file.txt", content); err != nil {
		t.Fatalf("push: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	pulled, err := c.Pull(ctx, "test-file.txt")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}

	if string(pulled) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", pulled, content)
	}
}

func TestIntegration_PushPullBinary(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	content := make([]byte, 256)
	for i := range content {
		content[i] = byte(i)
	}

	if err := c.Push(ctx, "binary.dat", content); err != nil {
		t.Fatalf("push binary: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	pulled, err := c.Pull(ctx, "binary.dat")
	if err != nil {
		t.Fatalf("pull binary: %v", err)
	}

	if len(pulled) != len(content) {
		t.Fatalf("length mismatch: got %d, want %d", len(pulled), len(content))
	}
	for i := range content {
		if pulled[i] != content[i] {
			t.Errorf("byte %d: got %d, want %d", i, pulled[i], content[i])
			break
		}
	}
}

func TestIntegration_Exec(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	resp, err := c.Exec(ctx, "echo hello-relay", "", 10)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if resp.ExitCode != 0 {
		t.Errorf("exit code: %d", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "hello-relay") {
		t.Errorf("stdout: %q", resp.Stdout)
	}
}

func TestIntegration_Delete(t *testing.T) {
	watchDir := t.TempDir()

	testFile := filepath.Join(watchDir, "to-delete.txt")
	os.WriteFile(testFile, []byte("delete me"), 0644)

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	if err := c.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestIntegration_Ping(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestIntegration_Subscribe(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	if err := c.Subscribe(ctx, "test-watch"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

func TestIntegration_LargeFile(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	size := 1024 * 1024
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}

	h := sha256.Sum256(content)
	originalDigest := hex.EncodeToString(h[:])

	if err := c.Push(ctx, "large.bin", content); err != nil {
		t.Fatalf("push large: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	pulled, err := c.Pull(ctx, "large.bin")
	if err != nil {
		t.Fatalf("pull large: %v", err)
	}

	if len(pulled) != size {
		t.Fatalf("size mismatch: got %d, want %d", len(pulled), size)
	}

	h2 := sha256.Sum256(pulled)
	pulledDigest := hex.EncodeToString(h2[:])
	if pulledDigest != originalDigest {
		t.Errorf("digest mismatch: %s vs %s", pulledDigest, originalDigest)
	}
}

func TestIntegration_ConnectionPool(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	ctx := context.Background()

	c1, err := client.GetOrConnect(ctx, wsURL, "test-token", "test-watch")
	if err != nil {
		t.Fatalf("get or connect 1: %v", err)
	}

	c2, err := client.GetOrConnect(ctx, wsURL, "test-token", "test-watch")
	if err != nil {
		t.Fatalf("get or connect 2: %v", err)
	}

	if c1 != c2 {
		t.Error("pool should return the same client instance")
	}

	client.CloseAll()
}

func TestIntegration_PathTraversal(t *testing.T) {
	watchDir := t.TempDir()

	secretDir := filepath.Join(filepath.Dir(watchDir), "secret")
	os.MkdirAll(secretDir, 0755)
	os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("secret"), 0644)
	defer os.RemoveAll(secretDir)

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	_, err := c.Pull(ctx, "../secret/secret.txt")
	if err == nil {
		t.Error("expected error for path traversal, got nil")
	}

	fmt.Printf("path traversal error (expected): %v\n", err)
}
