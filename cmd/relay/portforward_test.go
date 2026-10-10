package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/user/relay/internal/relay/client"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/server"
)

func TestParsePortForwardSpec(t *testing.T) {
	tests := []struct {
		raw         string
		defaultBind string
		wantBind    string
		wantLocal   int
		wantRemoteH string
		wantRemoteP int
		wantErr     bool
	}{
		{
			raw:         "8080",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 8080,
			wantErr:     false,
		},
		{
			raw:         "8080:9000",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 9000,
			wantErr:     false,
		},
		{
			raw:         "0.0.0.0:8080:9000",
			defaultBind: "127.0.0.1",
			wantBind:    "0.0.0.0",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 9000,
			wantErr:     false,
		},
		{
			raw:         "8080:db.local:5432",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "db.local",
			wantRemoteP: 5432,
			wantErr:     false,
		},
		{
			raw:     "invalid",
			wantErr: true,
		},
		{
			raw:     "0:8080",
			wantErr: true,
		},
		{
			raw:     "8080:70000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		spec, err := parsePortForwardSpec(tt.raw, tt.defaultBind)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePortForwardSpec(%q) expected error, got nil", tt.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parsePortForwardSpec(%q) unexpected error: %v", tt.raw, err)
		}
		if spec.BindHost != tt.wantBind {
			t.Errorf("BindHost = %q, want %q", spec.BindHost, tt.wantBind)
		}
		if spec.LocalPort != tt.wantLocal {
			t.Errorf("LocalPort = %d, want %d", spec.LocalPort, tt.wantLocal)
		}
		if spec.RemoteHost != tt.wantRemoteH {
			t.Errorf("RemoteHost = %q, want %q", spec.RemoteHost, tt.wantRemoteH)
		}
		if spec.RemotePort != tt.wantRemoteP {
			t.Errorf("RemotePort = %d, want %d", spec.RemotePort, tt.wantRemoteP)
		}
	}
}

func TestPortForwardSpec_String(t *testing.T) {
	cases := []struct {
		spec *PortForwardSpec
		want string
	}{
		{&PortForwardSpec{LocalPort: 8080, RemotePort: 8080}, "8080"},
		{&PortForwardSpec{LocalPort: 8080, RemotePort: 9000}, "8080:9000"},
		{&PortForwardSpec{LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000}, "8080:9000"},
		{&PortForwardSpec{LocalPort: 8080, RemoteHost: "db.local", RemotePort: 5432}, "8080:db.local:5432"},
	}
	for _, c := range cases {
		got := c.spec.String()
		if got != c.want {
			t.Errorf("spec.String() = %q, want %q", got, c.want)
		}
	}
}

// 端到端: 测试 run -p 的前台端口转发集成流程
// 模拟执行方上的真实 HTTP/TCP 服务，中转服务器开启隧道，
// 通过 StartPortForwardsForeground 建立本地端口转发并发送真实流量验证全双工透传
func TestEndToEnd_RunPortForwardForeground(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 启动远端模拟 TCP 服务 (Echo 服务)
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

	// 2. 启动中转服务器 (开启隧道)
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

	// 3. 执行方(node-agent)注册到中转
	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-exe",
		"executor_id": "node-agent",
		"watch_dir":   ".",
		"executor":    true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 请求方作为客户端接入
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

	// 5. 获取一个可用的本地端口，并构建规格 (模拟 -p <localPort>:<remotePort>)
	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	localPort := testLn.Addr().(*net.TCPAddr).Port
	testLn.Close()

	specRaw := fmt.Sprintf("%d:%d", localPort, remotePort)
	spec, err := parsePortForwardSpec(specRaw, "127.0.0.1")
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	// 6. 启动前台端口转发 (模拟 runExec 在命令运行前拉起端口转发)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopPF, err := StartPortForwardsForeground(ctx, rb, "node-agent", []*PortForwardSpec{spec})
	if err != nil {
		t.Fatalf("start port forwards foreground: %v", err)
	}
	defer stopPF()

	// 7. 向本地转发端口发送请求，验证流量穿透中转送达远端 Echo 服务并返回
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local port forward %s: %v", localAddr, err)
	}
	defer conn.Close()

	testMsg := "hello-run-publish-port-forward\n"
	if _, err := conn.Write([]byte(testMsg)); err != nil {
		t.Fatalf("write to local port: %v", err)
	}

	buf := make([]byte, len(testMsg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from local port: %v", err)
	}

	if string(buf) != testMsg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), testMsg)
	}

	// 8. 验证停止端口转发后，本地监听成功关闭
	stopPF()
	time.Sleep(50 * time.Millisecond)
	_, dialErr := net.DialTimeout("tcp", localAddr, 200*time.Millisecond)
	if dialErr == nil {
		t.Fatalf("expected dial to fail after stopPF, but succeeded")
	}
}
