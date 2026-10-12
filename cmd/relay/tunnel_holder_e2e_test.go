package main

import (
	"context"
	"encoding/json"
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
	"github.com/user/relay/internal/relay/server"
)

// TestEndToEnd_TunnelHolder_SocksViaHubAndRestore 端到端验证 node 接入后的隧道持有者全链路:
//  1. 持有者经中转把本地 SOCKS5 CONNECT 转发到「出口 executor」可达的真实目标(双向字节泵);
//  2. node 重启时 restore() 会按 restart=always 重新绑定监听。
//
// 这条路径正是 node.go 在 agent 角色块内启动 tunnelHolder 后所触发的代码,故同时覆盖 Unit 2 的
// 「socket 就绪 / always 回放 / 监听可见」运行期断言。
func TestEndToEnd_TunnelHolder_SocksViaHubAndRestore(t *testing.T) {
	relaybackend.SetExecutorRole(true)
	defer relaybackend.SetExecutorRole(false)
	defer client.CloseAll()

	// 1. 出口侧可达的真实目标:一个 TCP Echo 服务。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen remote echo: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { defer conn.Close(); _, _ = io.Copy(conn, conn) }(c)
		}
	}()

	// 2. 中转服务器(开启隧道)。
	watchDir := t.TempDir()
	srv, err := server.New(server.Config{
		Addr:          ":0",
		WatchDirs:     []server.WatchDirConfig{{ID: "test", Dir: watchDir}},
		Auth:          server.AuthConfig{Type: "token", Tokens: []string{"tok-exe", "tok-req"}},
		TunnelEnabled: true,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	// 3. 出口 executor 注册(白名单放行 127.0.0.1,loopback 目标可被隧道建连)。
	if _, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":           wsURL,
		"token":         "tok-exe",
		"executor_id":   "eg-node",
		"watch_dir":     ".",
		"executor":      true,
		"network_allow": []interface{}{"127.0.0.1"},
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}

	// 4. 持有者侧请求后端。
	reqRaw, err := relaybackend.NewRelayBackend(map[string]interface{}{
		"url":         wsURL,
		"token":       "tok-req",
		"executor_id": "cli-tunnel",
		"watch_dir":   ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rb := reqRaw.(*relaybackend.RelayBackend)

	// 5. 在短路径临时目录启动持有者(macOS unix socket 路径上限 ~104 字节)。
	sockDir, err := os.MkdirTemp("/tmp", "relay-tun-e2e-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "tunnel.sock")
	statePath := filepath.Join(sockDir, "tunnels.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := newTunnelHolder(ctx, rb, sockPath)
	holder.statePath = statePath
	holderErr := make(chan error, 1)
	go func() { holderErr <- holder.Run() }()

	waitSockReady := func(p string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-holderErr:
				t.Fatalf("holder.Run returned early: %v", err)
			default:
			}
			if _, err := os.Stat(p); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("control socket %s did not appear", p)
	}
	waitSockReady(sockPath)

	// 6. 打开一条隧道:本地监听 -> 出口 eg-node。
	listenPort := freeTCPPort(t)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	if resp := sendTunControl(t, sockPath, tunControlMsg{Op: tunOpOpen, Egress: "eg-node", Listen: listenAddr, Restart: tunRestartNo}); !resp.OK {
		t.Fatalf("open tunnel: %s", resp.Error)
	}

	// 7. 经本地 SOCKS5 端口 CONNECT 到出口可达的 echo,验证真实双向透传。
	conn, rep := socks5ClientConnect(t, listenAddr, "127.0.0.1", echoPort)
	defer conn.Close()
	if rep != socksRepSuccess {
		t.Fatalf("SOCKS5 CONNECT rep = 0x%02x, want success", rep)
	}
	payload := "tunnel-into-node e2e\n"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo through tunnel: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}

	// 8. 模拟 node 重启后的 restore():用一个全新的持有者实例(独立 socket/state)证明
	//    restart=always 的意图会被重新绑定。(holder.Run 与 forwardHolder 一样以进程退出收尾,
	//    不依赖 ctx 取消关闭监听,故这里另起实例而非停掉上一个。)
	sockPath2 := filepath.Join(sockDir, "tunnel2.sock")
	statePath2 := filepath.Join(sockDir, "tunnels2.json")
	alwaysPort := freeTCPPort(t)
	alwaysAddr := fmt.Sprintf("127.0.0.1:%d", alwaysPort)
	seed, _ := json.Marshal([]tunnelState{{Egress: "eg-always", Listen: alwaysAddr, Restart: tunRestartAlways}})
	if err := os.WriteFile(statePath2, seed, 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	holder2 := newTunnelHolder(ctx2, rb, sockPath2)
	holder2.statePath = statePath2
	holder2Err := make(chan error, 1)
	go func() { holder2Err <- holder2.Run() }()

	// restore() 应已按 always 重新绑定监听:能拨通该端口即为证据。
	restored := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-holder2Err:
			t.Fatalf("holder2.Run returned early: %v", err)
		default:
		}
		if c, err := net.DialTimeout("tcp", alwaysAddr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			restored = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !restored {
		t.Fatalf("restore() did not reopen restart=always tunnel on %s", alwaysAddr)
	}

	// list 应包含该已恢复条目且为 running。
	listResp := sendTunControl(t, sockPath2, tunControlMsg{Op: tunOpList})
	if !listResp.OK {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	found := false
	for _, ts := range listResp.Tunnels {
		if ts.Egress == "eg-always" && ts.State == "running" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored tunnel not listed running: %+v", listResp.Tunnels)
	}
}

// socks5ClientConnect 作为 SOCKS5 客户端经本地代理地址向 targetHost:targetPort 发起 CONNECT,
// 返回建连后的连接与 REP(0x00 成功)。握手失败即 Fatal。
func socks5ClientConnect(t *testing.T, proxyAddr, targetHost string, targetPort int) (net.Conn, byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local SOCKS5 %s: %v", proxyAddr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	// 版本协商:VER=5, 1 method, no-auth(0x00)。
	if _, err := conn.Write([]byte{socksVer5, 0x01, 0x00}); err != nil {
		conn.Close()
		t.Fatalf("write greeting: %v", err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, methodReply); err != nil {
		conn.Close()
		t.Fatalf("read method reply: %v", err)
	}
	ip := net.ParseIP(targetHost).To4()
	if ip == nil {
		conn.Close()
		t.Fatalf("target host %q is not IPv4", targetHost)
	}
	req := []byte{socksVer5, socksCmdConnect, 0x00, 0x01, ip[0], ip[1], ip[2], ip[3], byte(targetPort >> 8), byte(targetPort)}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		t.Fatalf("read CONNECT reply: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply[1]
}
