package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tunnelInstanceName 应把 executor 映射为 `<prefix>-<executor>` 实例名,并净化非法文件名字符。
func TestTunnelInstanceName(t *testing.T) {
	cases := []struct {
		executor, want string
	}{
		{"node-a", "tunnel-node-a"},
		{"a/b:c d", "tunnel-a_b_c_d"}, // 路径分隔/冒号/空格 → 下划线
		{"中文字符", "tunnel-____"},       // 4 个非 [A-Za-z0-9._-] 全净化(前缀连字符保留)
	}
	for _, c := range cases {
		if got := tunnelInstanceName(c.executor); got != c.want {
			t.Errorf("tunnelInstanceName(%q) = %q, want %q", c.executor, got, c.want)
		}
	}
}

// tunnelRestartPolicyValue 应把 --restart/--no-restart 归一为 always|no,--no-restart 优先,
// 缺省为 no,非法值报错。直接置位包级旗标指针并在 defer 中还原。
func TestTunnelRestartPolicyValue(t *testing.T) {
	origPolicy, origNoRestart := *tunnelRestartPolicy, *tunnelNoRestart
	defer func() { *tunnelRestartPolicy, *tunnelNoRestart = origPolicy, origNoRestart }()

	cases := []struct {
		name      string
		policy    string
		noRestart bool
		want      string
		wantErr   bool
	}{
		{name: "no-restart flag", policy: "always", noRestart: true, want: tunRestartNo},
		{name: "default empty", policy: "", want: tunRestartNo},
		{name: "always", policy: "always", want: tunRestartAlways},
		{name: "no", policy: "no", want: tunRestartNo},
		{name: "trim and lowercase", policy: "  ALWAYS ", want: tunRestartAlways},
		{name: "invalid", policy: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*tunnelRestartPolicy = tc.policy
			*tunnelNoRestart = tc.noRestart
			got, err := tunnelRestartPolicyValue()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("tunnelRestartPolicyValue(%q, noRestart=%v) = %q, want error", tc.policy, tc.noRestart, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("tunnelRestartPolicyValue(%q, noRestart=%v): %v", tc.policy, tc.noRestart, err)
			}
			if got != tc.want {
				t.Errorf("tunnelRestartPolicyValue(%q, noRestart=%v) = %q, want %q", tc.policy, tc.noRestart, got, tc.want)
			}
		})
	}
}

// handshakeOverTCP 用真实 TCP 连接跑一次 SOCKS5 握手(SOCKS 客户端在服务端侧),
// 返回解析出的目标。TCP 全双工避免 net.Pipe 的同步阻塞问题。
func handshakeOverTCP(t *testing.T, req []byte) (string, uint16, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write(req) // 客户端一次性把握手+CONNECT 发出去
		// 读握手 2 字节回复(VER, METHOD)
		buf := make([]byte, 2)
		_, _ = c.Read(buf)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	host, port, _, err := socks5Handshake(conn, bufio.NewReader(conn))
	return host, port, err
}

func TestSocks5Handshake_AddrTypes(t *testing.T) {
	cases := []struct {
		name     string
		req      []byte
		wantHost string
		wantPort uint16
	}{
		{
			name:     "ipv4",
			req:      []byte{0x05, 0x01, 0x00, 0x05, 0x01, 0x00, 0x01, 192, 0, 2, 1, 0x1f, 0x90},
			wantHost: "192.0.2.1",
			wantPort: 8080,
		},
		{
			name:     "domain",
			req:      []byte{0x05, 0x01, 0x00, 0x05, 0x01, 0x00, 0x03, 7, 'a', 'p', 'i', '.', 'i', 'n', 't', 0x01, 0xbb},
			wantHost: "api.int",
			wantPort: 443,
		},
		{
			name:     "ipv6",
			req:      []byte{0x05, 0x01, 0x00, 0x05, 0x01, 0x00, 0x04, 0x20, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x00, 0x50},
			wantHost: "2001::1",
			wantPort: 80,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := handshakeOverTCP(t, tc.req)
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("got %s:%d, want %s:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestSocks5Handshake_BadVersion(t *testing.T) {
	// 版本 4 应被拒。
	req := []byte{0x04, 0x01, 0x00}
	if _, _, err := handshakeOverTCP(t, req); err == nil {
		t.Fatal("expected error for bad SOCKS version, got nil")
	}
}

func TestSocks5Handshake_NonConnect(t *testing.T) {
	// 命令 2(BIND)应被拒。
	req := []byte{0x05, 0x01, 0x00, 0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}
	if _, _, err := handshakeOverTCP(t, req); err == nil {
		t.Fatal("expected error for non-CONNECT command, got nil")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1080", true},
		{"localhost:1080", true},
		{":1080", true},
		{"[::1]:1080", true},
		{"0.0.0.0:1080", false},
		{"192.168.1.10:1080", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestRunTunnelMode_CreatesTunnelAndPersistsIntent 验证新入口的创建动作落到既有 holder 上:
// 按 relay run --type=tunnel 解析出的请求下发 open 后,监听真的起来、意图进 tunnels.json。
// 用 in-process 测试 holder(非用户正在跑的 node),不依赖真机 node。
func TestRunTunnelMode_CreatesTunnelAndPersistsIntent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	_, _, sock := startTestHolder(t, statePath, io.Discard)

	port := freeTCPPort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	req, err := resolveTunnelMode(tunnelModeInput{
		Publish:  []string{listen},
		Executor: "site-a",
		BindHost: "127.0.0.1",
		Restart:  tunRestartAlways,
	})
	if err != nil {
		t.Fatalf("resolveTunnelMode: %v", err)
	}

	var warn bytes.Buffer
	if err := openTunnelViaHolder(sock, req.Egress, req.Listen.Addr(), req.Restart, &warn); err != nil {
		t.Fatalf("openTunnelViaHolder: %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning on loopback bind: %q", warn.String())
	}

	// 监听地址确实在监听。
	conn, err := net.DialTimeout("tcp", req.Listen.Addr(), time.Second)
	if err != nil {
		t.Fatalf("dial tunnel listener %s: %v", req.Listen.Addr(), err)
	}
	_ = conn.Close()

	// 意图已落盘,且 restart 策略随请求写入。
	states, err := loadTunnels(statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	var got *tunnelState
	for i := range states {
		if states[i].Egress == "site-a" {
			got = &states[i]
		}
	}
	if got == nil {
		t.Fatalf("tunnels.json has no entry for egress site-a: %+v", states)
	}
	if got.Listen != req.Listen.Addr() || got.Restart != tunRestartAlways || got.State != "running" {
		t.Errorf("persisted state = %+v, want listen=%s restart=%s state=running", got, req.Listen.Addr(), tunRestartAlways)
	}
}

// TestRunTunnelMode_NonLoopbackWarnsButCreates 验证非 loopback 绑定:告警打到调用方的
// writer(生产即 stderr),隧道照旧建成(R17)。
func TestRunTunnelMode_NonLoopbackWarnsButCreates(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "tunnels.json")
	_, _, sock := startTestHolder(t, statePath, io.Discard)

	port := freeTCPPort(t)
	listen := fmt.Sprintf("0.0.0.0:%d", port)
	req, err := resolveTunnelMode(tunnelModeInput{
		Publish:  []string{listen},
		Executor: "site-b",
		BindHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("resolveTunnelMode: %v", err)
	}

	var warn bytes.Buffer
	if err := openTunnelViaHolder(sock, req.Egress, req.Listen.Addr(), req.Restart, &warn); err != nil {
		t.Fatalf("openTunnelViaHolder: %v", err)
	}
	if !strings.Contains(warn.String(), "WARNING") || !strings.Contains(warn.String(), "non-loopback") {
		t.Errorf("warning = %q, want a non-loopback WARNING", warn.String())
	}
	if !strings.Contains(warn.String(), listen) {
		t.Errorf("warning = %q, want it to name %s", warn.String(), listen)
	}

	states, err := loadTunnels(statePath)
	if err != nil {
		t.Fatalf("loadTunnels: %v", err)
	}
	found := false
	for _, st := range states {
		if st.Egress == "site-b" && st.Listen == listen && st.State == "running" {
			found = true
		}
	}
	if !found {
		t.Errorf("tunnel not created for %s: %+v", listen, states)
	}
}
