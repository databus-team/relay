package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 原子替换:目标内容应变成新二进制内容。
func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "relay")
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "new")
	if err := os.WriteFile(src, []byte("new-binary-contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBinary(dst, src); err != nil {
		t.Fatalf("ReplaceBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary-contents" {
		t.Fatalf("binary content after replace = %q", got)
	}
	// 权限应被设为可执行
	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced binary not executable: %v", info.Mode())
	}
	// 空源应报错
	if err := ReplaceBinary(dst, filepath.Join(dir, "empty")); err == nil {
		t.Error("expected error when replacing from empty/nonexistent source")
	}
}

// Get 状态探测:默认 stopped;损坏 pid 视为 stopped;自身 pid 视为 running。
func TestStatusProbe(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "x.pid")

	if st := Get(pidFile); st.Running || st.Err != nil {
		t.Fatalf("no pid file: want stopped, got %+v", st)
	}
	if err := os.WriteFile(pidFile, []byte("notanumber"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := Get(pidFile); st.Running {
		t.Fatalf("corrupt pid: want stopped, got %+v", st)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := Get(pidFile); !st.Running || st.Pid != os.Getpid() {
		t.Fatalf("own pid: want running, got %+v", st)
	}
}

// replaceBinary 不应写入空"目录"等边界。
func TestReplaceBinaryRejectsDir(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "relay")
	os.WriteFile(dst, []byte("x"), 0o755)
	if err := ReplaceBinary(dst, dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected dir-rejection error, got %v", err)
	}
}
