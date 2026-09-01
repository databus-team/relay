package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
)

func setupTestServerAuth(t *testing.T, watchDir string, tokens []string) (*httptest.Server, string) {
	t.Helper()

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{
			Type:   "token",
			Tokens: tokens,
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

func setupTestServer(t *testing.T, watchDir string) (*httptest.Server, string) {
	t.Helper()
	return setupTestServerAuth(t, watchDir, []string{"test-token"})
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

// registerExecutorClient 连接一个充当「执行方」的客户端:为 test-watch 注册 executor,
// 收到入站 exec 时逐行流式写出两行后收尾。
func registerExecutorClient(t *testing.T, wsURL string) *client.Client {
	t.Helper()
	ctx := context.Background()

	exec := connectTestClient(t, wsURL)
	exec.SetExecHandler(func(sess *client.ExecSession) {
		go func() {
			_ = sess.Write(true, sess.Cmd()+"\n")
			_ = sess.Write(true, "world\n")
			_ = sess.Done(protocol.ExecResponse{ExitCode: 0})
		}()
	})
	if err := exec.RegisterExecutor(ctx, "test-watch", "add", "test-build-1"); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	return exec
}

func TestIntegration_Exec(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	exec := registerExecutorClient(t, wsURL)
	defer exec.Disconnect()

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

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

func TestIntegration_ExecStream(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	exec := registerExecutorClient(t, wsURL)
	defer exec.Disconnect()

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	type chunk struct {
		stdout bool
		data   string
	}
	var chunks []chunk

	resp, err := c.ExecStream(ctx, "whatever", "", 10, func(ch protocol.ExecChunk) {
		chunks = append(chunks, chunk{stdout: ch.Stdout, data: ch.Data})
	})
	if err != nil {
		t.Fatalf("exec stream: %v", err)
	}

	if resp.ExitCode != 0 {
		t.Errorf("exit code: %d", resp.ExitCode)
	}
	// 流式两行输出都应被逐块收到
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d: %+v", len(chunks), chunks)
	}
	if !strings.Contains(resp.Stdout, "whatever") || !strings.Contains(resp.Stdout, "world") {
		t.Errorf("stdout: %q", resp.Stdout)
	}
}

func TestIntegration_ExecNoExecutor(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	ctx := context.Background()

	if _, err := c.Exec(ctx, "echo x", "", 10); err == nil {
		t.Error("expected error when no executor registered, got nil")
	} else if !strings.Contains(err.Error(), "no executor") {
		t.Errorf("unexpected error: %v", err)
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

// TestIntegration_Version:中转版本台账应含 transit 自身 + 已注册执行方。
func TestIntegration_Version(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	exec := registerExecutorClient(t, wsURL)
	defer exec.Disconnect()

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	vr, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("version query: %v", err)
	}
	if !vr.OK {
		t.Fatalf("version not OK: %s", vr.Error)
	}

	var hasTransit, hasExecutor bool
	for _, n := range vr.Nodes {
		switch n.Role {
		case "transit":
			hasTransit = true
			if n.Version == "" || n.GOOS == "" {
				t.Errorf("transit node missing version/goos: %+v", n)
			}
		case "executor":
			if n.WatchID == "test-watch" && n.Version == "test-build-1" {
				hasExecutor = true
			}
		}
	}
	if !hasTransit {
		t.Error("version台账 should include a transit node")
	}
	if !hasExecutor {
		t.Error("version台账 should include the registered executor for test-watch")
	}
}

// TestIntegration_Transport:纯下发(Jobs=false)应把内容写到执行端目标路径、不跑 job。
func TestIntegration_Transport(t *testing.T) {
	watchDir := t.TempDir()

	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	// 执行方客户端:注册为 test-watch,并安装 push 处理器(Jobs=false 时把内容写到目标路径)。
	exec := connectTestClient(t, wsURL)
	exec.SetPushJobHandler(func(sess *client.PushJobSession) {
		dest := sess.RelPath
		os.MkdirAll(filepath.Dir(dest), 0o755)
		content, _ := os.ReadFile(sess.Temp.Name())
		if err := os.WriteFile(dest, content, 0o755); err != nil {
			_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: err.Error()})
			return
		}
		_ = sess.Done(protocol.ExecResponse{ExitCode: 0})
	})
	if err := exec.RegisterExecutor(context.Background(), "test-watch", "add", "tx-test"); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	defer exec.Disconnect()

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	dest := filepath.Join(t.TempDir(), "payload.bin")
	content := []byte("transport-bytes-123")
	resp, err := c.Transport(ctx, "test-watch", dest, content)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("transport exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("dest content = %q, want %q", got, content)
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

// TestIntegration_ReconnectConcurrentWrites 回归:重连不得重复启动 writeLoop。
// 若重新起一个写 goroutine,两个写 goroutine 会并发写同一 conn 而 panic
// (gorilla "concurrent write to websocket connection")。断连→重连→并发请求应无 panic。
func TestIntegration_ReconnectConcurrentWrites(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	ctx := context.Background()
	c, err := client.New(wsURL, "test-token", "test-watch")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	c.SetReconnectEnabled(true)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Disconnect()

	// 主动断连 → readLoop 报错 → 触发 reconnectLoop。等它重连成功。
	c.Disconnect()
	deadline := time.Now().Add(6 * time.Second)
	for !c.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("client did not reconnect in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 重连后并发发起一批请求;若残留了第二个 writeLoop 会立刻 panic。
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 多数并发写,让 writeLoop 并发路径被覆盖(-race 下更易暴露)。
			_ = c.Ping(context.Background())
		}()
	}
	wg.Wait()
}

// upgradeOverWS 用裸 websocket 驱动一次「服务器自升级」协议交互:连接 → 发 MsgServerUpgrade
// 头 → 流式上传 content(分块 + 摘要)→ 回读直到 MsgResponse(成功)/MsgError(失败)。
// 返回 (ok, errMsg);ok=true 表示收到成功 ACK(AE4 的 ACK 早于换装)。
func upgradeOverWS(t *testing.T, wsURL, token string, content []byte, digest string) (bool, string) {
	t.Helper()
	ctx := context.Background()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 握手:connect + token
	if err := conn.WriteJSON(protocol.Message{Type: protocol.MsgConnect, ID: "up-conn", Payload: protocol.ConnectRequest{ClientID: "upgrade-client", Token: token, Version: 1}}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	var ack protocol.Message
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connect ack: %v", err)
	}

	streamID := "stream-up-" + fmt.Sprint(time.Now().UnixNano())
	header := protocol.Message{
		Type:     protocol.MsgServerUpgrade,
		ID:       "up-1",
		StreamID: streamID,
		Payload: protocol.ServerUpgradeRequest{
			WatchID:  "test-watch",
			Size:     int64(len(content)),
			Digest:   digest,
			StreamID: streamID,
		},
	}
	if err := conn.WriteJSON(header); err != nil {
		t.Fatalf("write upgrade header: %v", err)
	}

	// 分块流式上传内容(压缩二进制帧)。
	if len(content) > 0 {
		chunkSize := protocol.DefaultChunkSize
		var offset int64
		chunk := 0
		for offset < int64(len(content)) {
			end := offset + int64(chunkSize)
			if end > int64(len(content)) {
				end = int64(len(content))
			}
			piece := content[offset:end]
			if err := conn.WriteJSON(protocol.Message{
				Type:     protocol.MsgStreamData,
				ID:       uuid.New().String(),
				StreamID: streamID,
				Payload:  protocol.StreamData{StreamID: streamID, Offset: offset, Chunk: chunk},
			}); err != nil {
				t.Fatalf("write stream data: %v", err)
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, protocol.Compress(piece)); err != nil {
				t.Fatalf("write binary: %v", err)
			}
			offset = end
			chunk++
		}
	}

	endMsg := protocol.Message{
		Type:     protocol.MsgStreamEnd,
		ID:       uuid.New().String(),
		StreamID: streamID,
		Payload:  protocol.StreamEnd{StreamID: streamID, OK: true, Received: int64(len(content)), Digest: digest},
	}
	if err := conn.WriteJSON(endMsg); err != nil {
		t.Fatalf("write stream end: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		switch m.Type {
		case protocol.MsgResponse:
			return true, ""
		case protocol.MsgError:
			errMsg, _ := m.Payload.(string)
			return false, errMsg
		}
	}
}

// TestIntegration_ServerUpgrade_NoToken:服务器未配置 token → 升级通道默认关闭(AE3)。
func TestIntegration_ServerUpgrade_NoToken(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := setupTestServerAuth(t, watchDir, nil)
	defer ts.Close()

	ok, errMsg := upgradeOverWS(t, wsURL, "", []byte("x"), "abc")
	if ok {
		t.Fatal("expected upgrade channel to be disabled without token, got ACK")
	}
	if !strings.Contains(errMsg, "disabled") {
		t.Errorf("unexpected error: %q", errMsg)
	}
}

// newSpySwapServer 建一个装有「换装 spy」的中转 server:换装一旦被调用就往 ch 里写。
func newSpySwapServer(t *testing.T, watchDir string) (*httptest.Server, string, chan string) {
	t.Helper()
	cfg := server.Config{
		Addr:      ":0",
		WatchDirs: []server.WatchDirConfig{{ID: "test-watch", Dir: watchDir}},
		Auth:      server.AuthConfig{Type: "token", Tokens: []string{"test-token"}},
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	spy := make(chan string, 1)
	srv.SetUpgradeSwap(func(tmpBin string) { spy <- tmpBin })
	ts := httptest.NewServer(srv)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
	return ts, wsURL, spy
}

// assertNoSwap 断言换装 spy 未被调用(校验/自检失败不得进入换装)。
func assertNoSwap(t *testing.T, spy chan string) {
	t.Helper()
	select {
	case <-spy:
		t.Fatal("swap hook must NOT be invoked on this abort path")
	default:
	}
}

// TestIntegration_ServerUpgrade_DigestMismatch:摘要不符 → 中止且不换装(AE1)。
func TestIntegration_ServerUpgrade_DigestMismatch(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL, spy := newSpySwapServer(t, watchDir)
	defer ts.Close()

	// 内容允许但用错误摘要声明。
	content := []byte("some-bytes")
	ok, errMsg := upgradeOverWS(t, wsURL, "test-token", content, "0000000000000000000000000000000000000000000000000000000000000000")
	if ok {
		t.Fatal("expected digest mismatch to abort, got ACK")
	}
	if !strings.Contains(errMsg, "digest") {
		t.Errorf("unexpected error: %q", errMsg)
	}
	assertNoSwap(t, spy)
}

// TestIntegration_ServerUpgrade_SelfCheckFail:自检失败 → 不换装、保留现行二进制(AE2)。
func TestIntegration_ServerUpgrade_SelfCheckFail(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL, spy := newSpySwapServer(t, watchDir)
	defer ts.Close()

	// 正确的摘要,但内容是不可启动的垃圾字节(bin 非可执行/不可用 version 启动)。
	content := []byte("#!/bin/sh\nexit 3\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	ok, errMsg := upgradeOverWS(t, wsURL, "test-token", content, digest)
	if ok {
		t.Fatal("expected self-check failure to abort, got ACK")
	}
	if !strings.Contains(errMsg, "self-check") {
		t.Errorf("unexpected error: %q", errMsg)
	}
	assertNoSwap(t, spy)
}

// TestIntegration_ServerUpgrade_ACK:自检通过 → 先回执成功 ACK(AE4,换装前 U2 结算)。
func TestIntegration_ServerUpgrade_ACK(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real relay binary")
	}
	watchDir := t.TempDir()
	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	// 构建一份真实 relay 二进制作为「可启动」负载,供自检通过路径使用。
	bin := filepath.Join(t.TempDir(), "relay-upgrade-test")
	build := exec.Command("go", "build", "-o", bin, "github.com/user/relay/cmd/relay")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build relay for upgrade test: %v\n%s", err, out)
	}
	content, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read built binary: %v", err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	ok, errMsg := upgradeOverWS(t, wsURL, "test-token", content, digest)
	if !ok {
		t.Fatalf("expected self-check pass + ACK, got error: %s", errMsg)
	}
}

// TestIntegration_ServerUpgrade_Swap:自检通过并回执 ACK 后,换装闭包被以已校验二进制调用
// (U3 换装链路:ACK 先行,随后换装收到同一二进制)。
func TestIntegration_ServerUpgrade_Swap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real relay binary")
	}
	watchDir := t.TempDir()

	cfg := server.Config{
		Addr: ":0",
		WatchDirs: []server.WatchDirConfig{
			{ID: "test-watch", Dir: watchDir},
		},
		Auth: server.AuthConfig{Type: "token", Tokens: []string{"test-token"}},
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	var swapped []byte
	var swapMu sync.Mutex
	var swapCalled = make(chan struct{}, 1)
	srv.SetUpgradeSwap(func(tmpBin string) {
		b, _ := os.ReadFile(tmpBin)
		swapMu.Lock()
		swapped = b
		swapMu.Unlock()
		select {
		case swapCalled <- struct{}{}:
		default:
		}
	})

	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	// 构建一份真实 relay 二进制作为「可启动」负载。
	bin := filepath.Join(t.TempDir(), "relay-upgrade-test")
	build := exec.Command("go", "build", "-o", bin, "github.com/user/relay/cmd/relay")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build relay for upgrade test: %v\n%s", err, out)
	}
	content, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read built binary: %v", err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	ok, errMsg := upgradeOverWS(t, wsURL, "test-token", content, digest)
	if !ok {
		t.Fatalf("expected ACK, got error: %s", errMsg)
	}

	// 换装闭包应已以相同二进制被调用。
	select {
	case <-swapCalled:
	case <-time.After(10 * time.Second):
		t.Fatal("swap hook was not invoked after successful ACK")
	}
	swapMu.Lock()
	defer swapMu.Unlock()
	if len(swapped) != len(content) || string(swapped) != string(content) {
		t.Errorf("swap received a binary that differs from the one verified")
	}
}

// TestIntegration_ClientUpgradeServer 客户端 UpgradeServer 全链路(上传 → 自检 → ACK)
// 成功;server 未接线换装 → 仅回 ACK,U4-T1 传输结算点。
func TestIntegration_ClientUpgradeServer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real relay binary")
	}
	watchDir := t.TempDir()
	ts, wsURL := setupTestServer(t, watchDir)
	defer ts.Close()

	bin := filepath.Join(t.TempDir(), "relay-upgrade-test")
	build := exec.Command("go", "build", "-o", bin, "github.com/user/relay/cmd/relay")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build relay: %v\n%s", err, out)
	}
	content, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	if err := c.UpgradeServer(ctx, content); err != nil {
		t.Fatalf("UpgradeServer: %v", err)
	}
}

// TestIntegration_ClientUpgradeServer_NoToken:中转未配置 token → 升级通道默认关闭,
// UpgradeServer 应返回可读错误(U4-T2/AE3)。
func TestIntegration_ClientUpgradeServer_NoToken(t *testing.T) {
	watchDir := t.TempDir()
	ts, wsURL := setupTestServerAuth(t, watchDir, nil)
	defer ts.Close()

	ctx := context.Background()
	c := connectTestClient(t, wsURL)
	defer c.Disconnect()

	err := c.UpgradeServer(ctx, []byte{0x00, 0x01})
	if err == nil {
		t.Fatal("expected error when transit has no token, got nil")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("unexpected error: %v", err)
	}
}
