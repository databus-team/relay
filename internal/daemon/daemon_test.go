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

// U3-T1: 替换前已生成 `<exe>.prev`(含旧版本构建)。
func TestReplaceBinaryWithKeep_CreatesPrev(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "relay")
	os.WriteFile(dst, []byte("old-binary-v1"), 0o755)
	src := filepath.Join(dir, "new")
	os.WriteFile(src, []byte("new-binary-v2"), 0o644)

	if err := ReplaceBinaryWithKeep(dst, src); err != nil {
		t.Fatalf("ReplaceBinaryWithKeep: %v", err)
	}

	// 目标已更新。
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary-v2" {
		t.Fatalf("dst = %q, want new-binary-v2", got)
	}

	// `.prev` 保留旧版构建。
	prev, err := os.ReadFile(dst + ".prev")
	if err != nil {
		t.Fatalf("read prev: %v", err)
	}
	if string(prev) != "old-binary-v1" {
		t.Fatalf("prev = %q, want old-binary-v1", prev)
	}
}

// U3-T2: 换装失败(替换源缺失)→ 不自动回滚,`.prev` 仍已保留。
func TestReplaceBinaryWithKeep_Failed_KeepsPrev(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "relay")
	os.WriteFile(dst, []byte("old-binary-v1"), 0o755)
	missing := filepath.Join(dir, "does-not-exist")

	// 备份应已发生(即使后面替换失败)。判断:替换失败返回错误,且 `.prev` 已落位。
	if err := ReplaceBinaryWithKeep(dst, missing); err == nil {
		t.Fatal("expected error replacing from missing source")
	}

	if _, err := os.Stat(dst + ".prev"); err != nil {
		t.Fatalf("expected .prev backup to be created even on failed swap: %v", err)
	}
	// 现行二进制不受影响。
	got, _ := os.ReadFile(dst)
	if string(got) != "old-binary-v1" {
		t.Fatalf("dst corrupted after failed swap: %q", got)
	}
}
