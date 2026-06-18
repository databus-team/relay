package integration

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
)

func TestEventDriven_FileCreate(t *testing.T) {
	watchDir := t.TempDir()

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{Tokens: []string{"test-token"}},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	ctx := context.Background()
	if err := srv.StartFileWatcher(ctx); err != nil {
		t.Fatalf("start file watcher: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	if err := c.Subscribe(ctx, "test-watch"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	testFile := filepath.Join(watchDir, "event-test.txt")
	if err := os.WriteFile(testFile, []byte("hello event"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	select {
	case event := <-c.EventCh():
		if event.Name != "event-test.txt" {
			t.Errorf("event name: got %q, want %q", event.Name, "event-test.txt")
		}
		if event.Op != protocol.OpCreate && event.Op != protocol.OpModify {
			t.Errorf("event op: got %q, want create or modify", event.Op)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for file event")
	}
}

func TestEventDriven_FileModify(t *testing.T) {
	watchDir := t.TempDir()

	testFile := filepath.Join(watchDir, "existing.txt")
	os.WriteFile(testFile, []byte("initial"), 0644)

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{Tokens: []string{"test-token"}},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	ctx := context.Background()
	if err := srv.StartFileWatcher(ctx); err != nil {
		t.Fatalf("start file watcher: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	if err := c.Subscribe(ctx, "test-watch"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(testFile, []byte("modified content"), 0644); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	select {
	case event := <-c.EventCh():
		if event.Name != "existing.txt" {
			t.Errorf("event name: got %q, want %q", event.Name, "existing.txt")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for modify event")
	}
}

func TestEventDriven_FileDelete(t *testing.T) {
	watchDir := t.TempDir()

	testFile := filepath.Join(watchDir, "to-remove.txt")
	os.WriteFile(testFile, []byte("remove me"), 0644)

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{Tokens: []string{"test-token"}},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	ctx := context.Background()
	if err := srv.StartFileWatcher(ctx); err != nil {
		t.Fatalf("start file watcher: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	if err := c.Subscribe(ctx, "test-watch"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	os.Remove(testFile)

	select {
	case event := <-c.EventCh():
		if event.Name != "to-remove.txt" {
			t.Errorf("event name: got %q, want %q", event.Name, "to-remove.txt")
		}
		if event.Op != protocol.OpDelete && event.Op != protocol.OpRename {
			t.Errorf("event op: got %q, want delete or rename", event.Op)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for delete event")
	}
}

func TestEventDriven_MultipleEvents(t *testing.T) {
	watchDir := t.TempDir()

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{Tokens: []string{"test-token"}},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	ctx := context.Background()
	if err := srv.StartFileWatcher(ctx); err != nil {
		t.Fatalf("start file watcher: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	if err := c.Subscribe(ctx, "test-watch"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		os.WriteFile(filepath.Join(watchDir, name), []byte(name), 0644)
	}

	received := make(map[string]bool)
	timeout := time.After(5 * time.Second)
	for len(received) < 3 {
		select {
		case event := <-c.EventCh():
			received[event.Name] = true
		case <-timeout:
			t.Fatalf("timeout: received %d/3 events: %v", len(received), received)
		}
	}

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if !received[name] {
			t.Errorf("missing event for %s", name)
		}
	}
}
