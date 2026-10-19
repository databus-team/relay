package main

import (
	"os"
	"path/filepath"
	"testing"
)

// unix socket 路径在 macOS 上约 104 字节,TempDir 太长会 bind: invalid argument,
// 故与现有 holder 测试一致,用 /tmp 下的短目录。
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "relay-sock-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// 已有 node 在服务同一控制 socket 时,第二个 holder 必须报错退出,不能摘掉重绑。
func TestListenHolderSocketRejectsLiveHolder(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "forward.sock")
	ln, err := listenHolderSocket(sock)
	if err != nil {
		t.Fatalf("first holder: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })

	if _, err := listenHolderSocket(sock); err == nil {
		t.Fatal("second holder should fail while the first is live")
	}
}

// 上一位进程已死、socket 文件只是陈旧残留时,应删除并重新绑定。
func TestListenHolderSocketReplacesStaleFile(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "forward.sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := listenHolderSocket(sock)
	if err != nil {
		t.Fatalf("stale socket file should be replaced: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })
}
