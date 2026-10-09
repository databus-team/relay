package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	relaybackend "github.com/user/relay/internal/relay/backend"
)

// PortForwardSpec 描述一条端口转发规则
type PortForwardSpec struct {
	BindHost   string
	LocalPort  int
	RemoteHost string
	RemotePort int
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

// resolveTargetExecutor 解析目标执行方节点标识
func resolveTargetExecutor(cfg *config.Config, execFlag, watchFlag string) (string, error) {
	if execFlag != "" {
		return execFlag, nil
	}
	w := watchFlag
	if w == "" {
		if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
			w = inferred
		}
	}
	if w != "" {
		watchCfg, err := cfg.GetWorkspaceByID(w)
		if err == nil && watchCfg.Executor != "" {
			return watchCfg.Executor, nil
		}
	}
	return "", fmt.Errorf("specify target executor with -e/--executor or bind to a workspace with -w")
}

// runPortForward 前台运行端口转发
func runPortForward() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Backend.Type != "relay" {
		fmt.Fprintf(os.Stderr, "Error: port-forward requires backend.type=relay, got %q\n", cfg.Backend.Type)
		os.Exit(1)
	}

	targetExec, err := resolveTargetExecutor(cfg, *portForwardExec, *portForwardWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if *portForwardPorts == "" {
		fmt.Fprintf(os.Stderr, "Error: port mapping is required (e.g. relay pf 8080 or relay pf 8080:8080)\n")
		os.Exit(1)
	}

	spec, err := parsePortForwardSpec(*portForwardPorts, *portForwardBind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}
	rb, ok := b.(*relaybackend.RelayBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: backend does not support relay tunnel\n")
		os.Exit(1)
	}

	listenAddr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Port-forward listen %s failed: %v\n", listenAddr, err)
		os.Exit(1)
	}
	defer ln.Close()

	if !isLoopbackAddr(spec.BindHost) {
		fmt.Fprintf(os.Stderr, "WARNING: listening on %q (non-loopback). This port has no auth.\n", listenAddr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nStopping port-forward...")
		cancel()
		ln.Close()
	}()

	fmt.Printf("Forwarding from %s -> %s:%d via %s (Ctrl-C to stop)\n", listenAddr, spec.RemoteHost, spec.RemotePort, targetExec)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go handlePortForwardConn(ctx, rb, targetExec, spec, conn)
	}
}

func handlePortForwardConn(ctx context.Context, rb *relaybackend.RelayBackend, targetExec string, spec *PortForwardSpec, conn net.Conn) {
	defer conn.Close()

	stream, err := rb.TunnelOpen(ctx, targetExec, spec.RemoteHost, uint16(spec.RemotePort))
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

// 实例名派生与后台守护进程管理
func pfInstanceName(executor string, localPort int) string {
	var b strings.Builder
	b.WriteString("pf-")
	for _, r := range executor {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString(fmt.Sprintf("-%d", localPort))
	return b.String()
}

func daemonStartPortForward() error {
	if *portForwardPorts == "" {
		return fmt.Errorf("port mapping is required (e.g. relay pf 8080 start)")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	targetExec, err := resolveTargetExecutor(cfg, *portForwardExec, *portForwardWatch)
	if err != nil {
		return err
	}
	spec, err := parsePortForwardSpec(*portForwardPorts, *portForwardBind)
	if err != nil {
		return err
	}
	instance := pfInstanceName(targetExec, spec.LocalPort)
	args := []string{
		"port-forward", *portForwardPorts, "run",
		"-e", targetExec,
		"--bind", spec.BindHost,
		"-c", *configPath,
	}
	return daemonStartArgs(instance, args)
}

func daemonStopPortForward() {
	if *portForwardPorts == "" {
		fmt.Fprintf(os.Stderr, "Error: port mapping is required (e.g. relay pf 8080 stop)\n")
		return
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	targetExec, err := resolveTargetExecutor(cfg, *portForwardExec, *portForwardWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	spec, err := parsePortForwardSpec(*portForwardPorts, *portForwardBind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	instance := pfInstanceName(targetExec, spec.LocalPort)
	daemonStop(instance)
}

func daemonStatusPortForward() error {
	if *portForwardPorts != "" && *portForwardExec != "" {
		spec, err := parsePortForwardSpec(*portForwardPorts, *portForwardBind)
		if err == nil {
			return daemonStatus(pfInstanceName(*portForwardExec, spec.LocalPort))
		}
	}
	insts, err := daemon.ListInstances("pf")
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		fmt.Println("No active port-forward instances found.")
		return nil
	}
	fmt.Printf("%-24s %-10s %-8s %s\n", "INSTANCE", "STATUS", "PID", "LOG")
	for _, inst := range insts {
		st := "stopped"
		pidStr := "-"
		if inst.Status.Running {
			st = "running"
			pidStr = strconv.Itoa(inst.Status.Pid)
		}
		fmt.Printf("%-24s %-10s %-8s %s\n", inst.Name, st, pidStr, inst.LogFile)
	}
	return nil
}
