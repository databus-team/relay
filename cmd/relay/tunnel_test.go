package main

import (
	"bufio"
	"net"
	"testing"
)

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
