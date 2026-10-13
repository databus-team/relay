package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
)

// 端到端: 测试 node-as-dockerd 端口转发持有者的 register -> 本地监听 -> 隧道透传 -> unregister 关闭流程。
// 模拟执行方上的 Echo 服务,中转服务器开启隧道,持有者通过 unix socket 接受 register 控制,
// 建立本地端口转发并发送真实流量验证全双工透传,再通过 unregister 回收监听。
func TestEndToEnd_ForwardHolder_RegisterEchoUnregister(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 远端模拟 TCP Echo 服务
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	remotePort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// 2. 中转服务器(开启隧道)
	watchDir := t.TempDir()
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
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	// 3. 执行方注册
	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-agent",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 请求方(持有者端口转转后端)
	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "cli-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	// 5. 取一个空闲本地端口
	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	localPort := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	// 6. 构造持有者并监听 unix 控制 socket(用一个短路径:macOS unix socket 有 ~104 字节路径上限)
	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "forward.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holder := newForwardHolder(ctx, rb, sockPath)
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- holder.Run() // 由 ctx 取消结束
	}()

	// 等待 socket 就绪
	waitSock := func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-holderErr:
				t.Fatalf("holder.Run returned: %v", err)
			default:
			}
			if _, err := os.Stat(sockPath); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("forward control socket %s did not appear", sockPath)
	}
	waitSock()

	// 7. register: 经 unix socket 提交 task 端口转发
	resp := sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-abc",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK {
		t.Fatalf("register failed: %s", resp.Error)
	}
	if len(resp.Registered) != 1 || resp.Registered[0] != localAddr {
		t.Fatalf("registered = %v, want [%s]", resp.Registered, localAddr)
	}

	// 8. 向本地转发端口发流,验证透传远端 Echo
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local forward %s: %v", localAddr, err)
	}
	testMsg := "hello-forward-holder\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to local port: %v", err)
	}
	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from local port: %v", err)
	}
	conn.Close()
	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}

	// 9. register 同一端口冲突:应 rejected 而不影响已有转发
	resp = sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-other",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK {
		t.Fatalf("conflicting register failed: %s", resp.Error)
	}
	if len(resp.Rejected) != 1 {
		t.Fatalf("conflicting register rejected = %v, want 1 rejection", resp.Rejected)
	}

	// 10. 验证端口仍可连(未被错误覆盖)
	conn2, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("port should still listen after conflict: %v", err)
	}
	conn2.Close()

	// 11. unregister: 关闭该 task 的全部转发
	unResp := sendControl(t, sockPath, fwdControlMsg{Op: opUnregister, TaskID: "task-abc"})
	if !unResp.OK {
		t.Fatalf("unregister failed: %s", unResp.Error)
	}

	// 12. 验证本地监听已关闭
	time.Sleep(50 * time.Millisecond)
	_, dialErr := net.DialTimeout("tcp", localAddr, 200*time.Millisecond)
	if dialErr == nil {
		t.Fatalf("expected dial to fail after unregister, but succeeded")
	}
}

// sendControl 通过 unix socket 发送一条控制消息并读取应答。
func sendControl(t *testing.T, sockPath string, msg fwdControlMsg) fwdControlResp {
	t.Helper()
	resp, err := sendForwardControl(sockPath, msg)
	if err != nil {
		t.Fatalf("send control %s: %v", msg.Op, err)
	}
	return resp
}

func TestCaptureDetachTaskID(t *testing.T) {
	cases := []struct {
		name   string
		data   string
		wantID string
		wantOK bool
	}{
		{"marker 精确解析", protocol.ForwardTaskIDMarker + "t-abc", "t-abc", true},
		{"marker 带空白", protocol.ForwardTaskIDMarker + "t-abc\n", "t-abc", true},
		{"marker 但空 ID 不捕获", protocol.ForwardTaskIDMarker + "   \n", "", false},
		{"非 marker 行不捕获", "t-old", "", false},
		{"空行不捕获", "\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := captureDetachTaskID(tc.data)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("captureDetachTaskID(%q) = (%q,%v), want (%q,%v)", tc.data, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestEndToEnd_ForwardHolder_RestoreOnRestart 验证 node restart 后 forwardHolder.restore()
// 自动恢复持久化的端口监听，并恢复透传。
func TestEndToEnd_ForwardHolder_RestoreOnRestart(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	remotePort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	watchDir := t.TempDir()
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
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-agent",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "cli-client",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	localPort := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	sockDir, err := os.MkdirTemp("/tmp", "relay-fwd-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "forward.sock")
	statePath := filepath.Join(sockDir, "forwards.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holder := newForwardHolder(ctx, rb, sockPath)
	holder.statePath = statePath
	go func() { _ = holder.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp := sendControl(t, sockPath, fwdControlMsg{
		Op:       opRegister,
		TaskID:   "task-restart-test",
		Executor: "node-agent",
		Specs: []PortForwardSpec{
			{BindHost: "127.0.0.1", LocalPort: localPort, RemoteHost: "127.0.0.1", RemotePort: remotePort},
		},
	})
	if !resp.OK || len(resp.Registered) != 1 {
		t.Fatalf("initial register failed: %+v", resp)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if !strings.Contains(string(data), "task-restart-test") {
		t.Fatalf("state file does not contain task id: %s", string(data))
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	holder.mu.Lock()
	for _, fw := range holder.byAddr {
		_ = fw.listener.Close()
	}
	holder.mu.Unlock()

	sockPath2 := filepath.Join(sockDir, "forward2.sock")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	holder2 := newForwardHolder(ctx2, rb, sockPath2)
	holder2.statePath = statePath
	go func() { _ = holder2.Run() }()

	deadline2 := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline2) {
		if _, err := os.Stat(sockPath2); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial restored forward %s failed: %v", localAddr, err)
	}
	testMsg := "hello-restored-forward\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to restored port: %v", err)
	}
	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from restored port: %v", err)
	}
	conn.Close()
	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}
}
