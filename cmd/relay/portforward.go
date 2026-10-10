package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	relaybackend "github.com/user/relay/internal/relay/backend"
)

// PortForwardSpec 描述一条端口转发规则
type PortForwardSpec struct {
	BindHost   string
	LocalPort  int
	RemoteHost string
	RemotePort int
}

// String 返回规格的标准字符串表示，例如 "8080"、"8080:9000" 或 "8080:db.local:5432"
func (s *PortForwardSpec) String() string {
	if s.RemoteHost != "" && s.RemoteHost != "127.0.0.1" && s.RemoteHost != "localhost" {
		return fmt.Sprintf("%d:%s:%d", s.LocalPort, s.RemoteHost, s.RemotePort)
	}
	if s.LocalPort == s.RemotePort {
		return strconv.Itoa(s.LocalPort)
	}
	return fmt.Sprintf("%d:%d", s.LocalPort, s.RemotePort)
}

// parsePortForwardSpec 解析形如 "8080", "8080:8080", "0.0.0.0:8080:8080", "8080:db.local:5432" 的规格
func parsePortForwardSpec(raw, defaultBind string) (*PortForwardSpec, error) {
	if defaultBind == "" {
		defaultBind = "127.0.0.1"
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 1:
		// "8080"
		p, err := strconv.Atoi(parts[0])
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("invalid port: %q", parts[0])
		}
		return &PortForwardSpec{
			BindHost:   defaultBind,
			LocalPort:  p,
			RemoteHost: "127.0.0.1",
			RemotePort: p,
		}, nil
	case 2:
		// "8080:9000"
		lp, err1 := strconv.Atoi(parts[0])
		rp, err2 := strconv.Atoi(parts[1])
		if err1 != nil || lp <= 0 || lp > 65535 {
			return nil, fmt.Errorf("invalid local port: %q", parts[0])
		}
		if err2 != nil || rp <= 0 || rp > 65535 {
			return nil, fmt.Errorf("invalid remote port: %q", parts[1])
		}
		return &PortForwardSpec{
			BindHost:   defaultBind,
			LocalPort:  lp,
			RemoteHost: "127.0.0.1",
			RemotePort: rp,
		}, nil
	case 3:
		// "0.0.0.0:8080:9000" 或 "8080:remotehost:9000"
		if lp, err := strconv.Atoi(parts[0]); err == nil {
			// 首段是数字: 8080:remotehost:9000
			rp, rerr := strconv.Atoi(parts[2])
			if rerr != nil || rp <= 0 || rp > 65535 {
				return nil, fmt.Errorf("invalid remote port: %q", parts[2])
			}
			return &PortForwardSpec{
				BindHost:   defaultBind,
				LocalPort:  lp,
				RemoteHost: parts[1],
				RemotePort: rp,
			}, nil
		}
		// 首段是 host: 0.0.0.0:8080:9000
		lp, err1 := strconv.Atoi(parts[1])
		rp, err2 := strconv.Atoi(parts[2])
		if err1 != nil || lp <= 0 || lp > 65535 || err2 != nil || rp <= 0 || rp > 65535 {
			return nil, fmt.Errorf("invalid port in %q", raw)
		}
		return &PortForwardSpec{
			BindHost:   parts[0],
			LocalPort:  lp,
			RemoteHost: "127.0.0.1",
			RemotePort: rp,
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized port format %q (expected [local:]remote or local:remoteHost:remotePort)", raw)
	}
}

// StartPortForwardsForeground 在前台启动端口转发监听，随 context 取消或 stop 回调关闭
func StartPortForwardsForeground(ctx context.Context, rb *relaybackend.RelayBackend, targetExec string, specs []*PortForwardSpec) (func(), error) {
	var listeners []net.Listener
	cleanup := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}

	for _, spec := range specs {
		listenAddr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("listen %s failed: %w", listenAddr, err)
		}
		listeners = append(listeners, ln)

		if !isLoopbackAddr(spec.BindHost) {
			fmt.Fprintf(os.Stderr, "WARNING: listening on %q (non-loopback). This port has no auth.\n", listenAddr)
		}
		fmt.Printf("➜ [port-forward] %s -> %s:%d via %s\n", listenAddr, spec.RemoteHost, spec.RemotePort, targetExec)

		go func(l net.Listener, s *PortForwardSpec) {
			go func() {
				<-ctx.Done()
				_ = l.Close()
			}()
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go handlePortForwardConn(ctx, rb, targetExec, s, conn)
			}
		}(ln, spec)
	}

	return cleanup, nil
}

func handlePortForwardConn(ctx context.Context, rb *relaybackend.RelayBackend, targetExec string, spec *PortForwardSpec, conn net.Conn) {
	defer conn.Close()

	stream, err := rb.TunnelOpenPortForward(ctx, targetExec, spec.RemoteHost, uint16(spec.RemotePort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[port-forward error] connect to %s:%d via %s failed: %v\n", spec.RemoteHost, spec.RemotePort, targetExec, err)
		return
	}
	defer stream.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// 本地 TCP -> 远端 TunnelStream
	go func() {
		defer wg.Done()
		defer stream.Close()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				piece := make([]byte, n)
				copy(piece, buf[:n])
				if werr := stream.Send(piece); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 远端 TunnelStream -> 本地 TCP
	go func() {
		defer wg.Done()
		defer conn.Close()
		for {
			d, rerr := stream.Recv()
			if rerr != nil {
				break
			}
			if _, werr := conn.Write(d); werr != nil {
				break
			}
		}
	}()

	wg.Wait()
}
