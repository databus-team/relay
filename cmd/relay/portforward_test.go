package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/client"
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
		// host:port 形态(R15): 绑定主机 + 本地端口，远端默认同本地端口
		{
			raw:         "127.0.0.1:8080",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 8080,
			wantErr:     false,
		},
		{
			raw:         "0.0.0.0:8080",
			defaultBind: "127.0.0.1",
			wantBind:    "0.0.0.0",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 8080,
			wantErr:     false,
		},
		// 显式 host 覆盖 --bind-host 默认值
		{
			raw:         "127.0.0.1:8080",
			defaultBind: "0.0.0.0",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 8080,
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
		{
			raw:     ":8080",
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

// `-p host:port` 与 `-p port --bind-host host` 等价，且解析结果必须无损带走绑定面(R21/KTD9):
// String() 只用于展示，记录必须走结构化字段。
func TestParsePortForwardSpec_HostPortEquivalentAndLossless(t *testing.T) {
	explicit, err := parsePortForwardSpec("127.0.0.1:8080", "127.0.0.1")
	if err != nil {
		t.Fatalf("parse 127.0.0.1:8080: %v", err)
	}
	viaFlag, err := parsePortForwardSpec("8080", "127.0.0.1")
	if err != nil {
		t.Fatalf("parse 8080: %v", err)
	}
	if *explicit != *viaFlag {
		t.Errorf("-p 127.0.0.1:8080 = %+v, want equivalent to -p 8080 --bind-host 127.0.0.1 = %+v", *explicit, *viaFlag)
	}

	// 绑定面不比默认窄：展示字符串丢掉 0.0.0.0，但结构化记录必须原样保留
	wild, err := parsePortForwardSpec("0.0.0.0:8080", "127.0.0.1")
	if err != nil {
		t.Fatalf("parse 0.0.0.0:8080: %v", err)
	}
	if wild.BindHost != "0.0.0.0" {
		t.Fatalf("BindHost = %q, want 0.0.0.0", wild.BindHost)
	}
	if wild.String() != "8080" {
		t.Errorf("String() = %q, want %q (display-only, does not carry the bind face)", wild.String(), "8080")
	}

	// 记录往返(生产侧以解析后的结构上行，不依赖 String())
	raw, err := json.Marshal(wild)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var restored PortForwardSpec
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if restored != *wild {
		t.Errorf("round-tripped spec = %+v, want %+v", restored, *wild)
	}
}

func TestParseTunnelListenSpec(t *testing.T) {
	tests := []struct {
		raw         string
		defaultBind string
		wantBind    string
		wantPort    int
		wantErr     bool
		errContains string
	}{
		{
			raw:         "1080",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantPort:    1080,
		},
		{
			raw:         "127.0.0.1:1080",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantPort:    1080,
		},
		{
			raw:         "0.0.0.0:1080",
			defaultBind: "127.0.0.1",
			wantBind:    "0.0.0.0",
			wantPort:    1080,
		},
		{
			raw:         "8080",
			defaultBind: "0.0.0.0",
			wantBind:    "0.0.0.0",
			wantPort:    8080,
		},
		{
			raw:         "8080:9000",
			defaultBind: "127.0.0.1",
			wantErr:     true,
			errContains: "[host:]port",
		},
		{
			raw:         "8080:9000:1080",
			defaultBind: "127.0.0.1",
			wantErr:     true,
			errContains: "[host:]port",
		},
		{
			raw:     "invalid",
			wantErr: true,
		},
		{
			raw:     "0",
			wantErr: true,
		},
		{
			raw:     "70000",
			wantErr: true,
		},
		{
			raw:         "127.0.0.1:70000",
			wantErr:     true,
			errContains: "[host:]port",
		},
		{
			raw:         ":8080",
			wantErr:     true,
			errContains: "[host:]port",
		},
	}

	for _, tt := range tests {
		spec, err := parseTunnelListenSpec(tt.raw, tt.defaultBind)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseTunnelListenSpec(%q) expected error, got nil", tt.raw)
				continue
			}
			if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("parseTunnelListenSpec(%q) error = %q, want it to contain %q", tt.raw, err.Error(), tt.errContains)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseTunnelListenSpec(%q) unexpected error: %v", tt.raw, err)
		}
		if spec.BindHost != tt.wantBind {
			t.Errorf("BindHost = %q, want %q", spec.BindHost, tt.wantBind)
		}
		if spec.Port != tt.wantPort {
			t.Errorf("Port = %d, want %d", spec.Port, tt.wantPort)
		}
	}
}

// `-p 1080` 与 `-p 127.0.0.1:1080` 必须得到同一个监听地址(绑定取 --bind-host)
func TestParseTunnelListenSpec_BarePortMatchesLoopback(t *testing.T) {
	bare, err := parseTunnelListenSpec("1080", "127.0.0.1")
	if err != nil {
		t.Fatalf("parse 1080: %v", err)
	}
	explicit, err := parseTunnelListenSpec("127.0.0.1:1080", "127.0.0.1")
	if err != nil {
		t.Fatalf("parse 127.0.0.1:1080: %v", err)
	}
	if *bare != *explicit {
		t.Errorf("-p 1080 -> %+v, want same listen address as -p 127.0.0.1:1080 -> %+v", *bare, *explicit)
	}
	if bare.Addr() != "127.0.0.1:1080" {
		t.Errorf("Addr() = %q, want %q", bare.Addr(), "127.0.0.1:1080")
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

// TestEndToEnd_RunPortForwardLargeStream 验证端口转发流式传输较大尺寸数据时(如 20MB)节点稳定,不会误报心跳超时或连接异常中断
func TestEndToEnd_RunPortForwardLargeStream(t *testing.T) {
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

	spec, err := parsePortForwardSpec(fmt.Sprintf("%d:%d", localPort, remotePort), "127.0.0.1")
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopPF, err := StartPortForwardsForeground(ctx, rb, "node-agent", []*PortForwardSpec{spec})
	if err != nil {
		t.Fatalf("start port forwards foreground: %v", err)
	}
	defer stopPF()

	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local port forward: %v", err)
	}
	defer conn.Close()

	// 传输 16MB 的数据流并校验哈希
	const totalBytes = 16 * 1024 * 1024
	sendHash := sha256.New()
	recvHash := sha256.New()

	errCh := make(chan error, 2)

	// 写协程
	go func() {
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = byte(i % 251)
		}
		var written int64
		for written < totalBytes {
			toWrite := int64(len(buf))
			if totalBytes-written < toWrite {
				toWrite = totalBytes - written
			}
			n, werr := conn.Write(buf[:toWrite])
			if n > 0 {
				sendHash.Write(buf[:n])
				written += int64(n)
			}
			if werr != nil {
				errCh <- fmt.Errorf("write error: %w", werr)
				return
			}
		}
		errCh <- nil
	}()

	// 读协程
	go func() {
		buf := make([]byte, 64*1024)
		var readBytes int64
		for readBytes < totalBytes {
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, rerr := conn.Read(buf)
			if n > 0 {
				recvHash.Write(buf[:n])
				readBytes += int64(n)
			}
			if rerr != nil {
				if readBytes < totalBytes {
					errCh <- fmt.Errorf("read error: %w (read %d/%d bytes)", rerr, readBytes, totalBytes)
					return
				}
				break
			}
		}
		errCh <- nil
	}()

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("stream transfer error: %v", err)
		}
	}

	sendSum := sendHash.Sum(nil)
	recvSum := recvHash.Sum(nil)
	if !bytes.Equal(sendSum, recvSum) {
		t.Fatalf("checksum mismatch: send %x != recv %x", sendSum, recvSum)
	}
}
