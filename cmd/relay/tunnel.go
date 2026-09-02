package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	relaybackend "github.com/user/relay/internal/relay/backend"
)

const (
	socksVer5       = 0x05
	socksCmdConnect = 0x01
	socksRepSuccess = 0x00
	socksRepFailure = 0x01
	socksRepReject  = 0x07 // command not supported

	// handshakeTimeout 本地 SOCKS5 握手上限:只连不发/半开连接在限定时间内结束。
	handshakeTimeout = 10 * time.Second
)

// runTunnel 本地 SOCKS5 出网隧道:监听本地端口,把每个客户端 CONNECT 目标的访问经中转转发到
// 所选 executor,由其白名单校验 + 真实连接后双向字节流往返。每隧道一进程、互不阻塞。
func runTunnel() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Backend.Type != "relay" {
		fmt.Fprintf(os.Stderr, "Error: tunnel requires backend.type=relay, got %q\n", cfg.Backend.Type)
		os.Exit(1)
	}

	// 出口 watch:显式 -w 优先;否则按当前目录推断(与其他命令一致)。缺省失败即报错退出。
	w := *tunnelWatch
	if w == "" {
		if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
			w = inferred
		}
	}
	if w == "" {
		fmt.Fprintf(os.Stderr, "Specify -w. Available: %s\n", joinAvailable(cfg.Watch))
		os.Exit(1)
	}
	if _, err := cfg.GetWatchByID(w); err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}
	rb, ok := b.(*relaybackend.RelayBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: tunnel requires a relay backend (got %T)\n", b)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", *tunnelListen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Tunnel listen: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	// 非 loopback 绑定会把这个「经执行方出内网」的能力暴露给任何能到达该端口的人,醒目警告。
	if !isLoopbackAddr(*tunnelListen) {
		fmt.Fprintf(os.Stderr, "WARNING: listening on %q (non-loopback). This tunnel has no auth; any host that can reach this port can use it to reach the executor's network.\n", *tunnelListen)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nShutting down tunnel...")
		cancel()
		ln.Close()
	}()

	fmt.Printf("tunnel: SOCKS5 on %s -> egress %s (Ctrl-C to stop)\n", *tunnelListen, w)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go handleTunnelConn(ctx, rb, w, conn)
	}
}

// handleTunnelConn 处理一条本地 SOCKS5 连接:SOCKS5 握手解 → TunnelOpen → 建连成功后
// 双向字节泵(本地 SOCKS ↔ 远端 executor 的真实连接)。任一侧失败时仅向 SOCKS 回失败帧,不建内网流量。
func handleTunnelConn(ctx context.Context, rb *relaybackend.RelayBackend, watchID string, soc net.Conn) {
	defer soc.Close()

	// 握手要有界:只连不发/半开的连接在限定时间内结束,不长期占用 fd/goroutine。
	br := bufio.NewReader(soc)
	_ = soc.SetDeadline(time.Now().Add(handshakeTimeout))
	host, port, br, err := socks5Handshake(soc, br)
	_ = soc.SetDeadline(time.Time{})
	if err != nil {
		if !errors.Is(err, io.EOF) {
			socks5Reply(soc, socksRepFailure)
		}
		return
	}

	stream, err := rb.TunnelOpen(ctx, watchID, host, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tunnel] %s:%d egress %s: %v\n", host, port, watchID, err)
		socks5Reply(soc, socksRepFailure)
		return
	}

	socks5Reply(soc, socksRepSuccess)

	var wg sync.WaitGroup
	wg.Add(2)
	// 本地 SOCKS → 远端:从与握手共享的 bufio.Reader 读,确保客户端随 CONNECT 管道化的首段
	// 应用字节不被握手的内部缓冲吞噬;读到字节即经隧道发往目标;读完/出错→#5 端隧道。
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := br.Read(buf)
			if n > 0 {
				if serr := stream.Send(buf[:n]); serr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		stream.Close()
	}()
	// 远端 → 本地 SOCKS:读到远端字节写回 SOCKS;隧道关闭则关闭对端。
	go func() {
		defer wg.Done()
		for {
			d, rerr := stream.Recv()
			if rerr != nil {
				break
			}
			if _, werr := soc.Write(d); werr != nil {
				break
			}
		}
		_ = soc.Close()
	}()
	wg.Wait()
}

// socks5Handshake 完成 RFC1928「无认证」握手并解析 CONNECT 目标,返回 (host, port, br)。
// 传入复用同一 bufio.Reader(握手后由调用方继续从 br 读取应用字节,避免管道化数据被封存在
// 握手的内部缓冲里被丢弃);回复中写入仍经原始 conn。
func socks5Handshake(conn net.Conn, br *bufio.Reader) (string, uint16, *bufio.Reader, error) {
	// 版本 + 认证方式协商
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return "", 0, br, err
	}
	if hdr[0] != socksVer5 {
		return "", 0, br, fmt.Errorf("bad SOCKS version %d", hdr[0])
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return "", 0, br, err
	}
	if _, err := conn.Write([]byte{socksVer5, 0x00}); err != nil { // 无认证
		return "", 0, br, err
	}

	// CONNECT 请求
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return "", 0, br, err
	}
	if req[0] != socksVer5 {
		return "", 0, br, fmt.Errorf("bad request version %d", req[0])
	}
	if req[1] != socksCmdConnect {
		return "", 0, br, errors.New("only CONNECT supported")
	}

	var host string
	switch req[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(br, ip); err != nil {
			return "", 0, br, err
		}
		host = fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
	case 0x04: // IPv6
		ip := make([]byte, 16)
		if _, err := io.ReadFull(br, ip); err != nil {
			return "", 0, br, err
		}
		host = net.IP(ip).String()
	case 0x03: // 域名
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return "", 0, br, err
		}
		domain := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, domain); err != nil {
			return "", 0, br, err
		}
		host = string(domain)
	default:
		return "", 0, br, fmt.Errorf("unsupported address type %d", req[3])
	}

	portb := make([]byte, 2)
	if _, err := io.ReadFull(br, portb); err != nil {
		return "", 0, br, err
	}
	return host, binary.BigEndian.Uint16(portb), br, nil
}

// socks5Reply 回写 SOCKS5 连接结果帧(REP)。
func socks5Reply(conn net.Conn, rep byte) {
	// VER REP RSV ATYP BND.ADDR BND.PORT(全部 0)
	_, _ = conn.Write([]byte{socksVer5, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}

// isLoopbackAddr 判断 listen 地址是否回环(仅本机);空 host 视为本机。
func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || strings.EqualFold(host, "localhost") || strings.EqualFold(host, "::1") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.Equal(net.IPv4(127, 0, 0, 1)))
}
