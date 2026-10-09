package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initTestGitRepo 在 tmp 目录初始化一个干净的 git 仓库并提交初始文件
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	ctx := context.Background()
	if _, err := runGit(ctx, dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello\nInitial content\n"), 0644)
	if _, err := runGit(ctx, dir, "config", "user.email", "test@relay.dev"); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if _, err := runGit(ctx, dir, "config", "user.name", "Relay Tester"); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if _, err := runGit(ctx, dir, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runGit(ctx, dir, "commit", "-m", "initial commit"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir
}

func TestInspectRepo_CleanAndDirty(t *testing.T) {
	// 1. 非 Git 目录
	tmp := t.TempDir()
	info, err := InspectRepo(tmp)
	if err != nil {
		t.Fatalf("InspectRepo non-git: %v", err)
	}
	if info.IsGitRepo {
		t.Errorf("expected IsGitRepo=false, got true")
	}

	// 2. 干净的 Git 仓库
	repo := initTestGitRepo(t)
	info, err = InspectRepo(repo)
	if err != nil {
		t.Fatalf("InspectRepo clean: %v", err)
	}
	if !info.IsGitRepo || info.IsDirty || info.BaseCommit == "" {
		t.Errorf("unexpected clean info: %+v", info)
	}

	// 3. 修改已跟踪文件 -> dirty
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Hello\nModified content\n"), 0644)
	info, err = InspectRepo(repo)
	if err != nil {
		t.Fatalf("InspectRepo dirty: %v", err)
	}
	if !info.IsDirty || info.UntrackedNum != 0 {
		t.Errorf("expected dirty with 0 untracked, got: %+v", info)
	}

	// 4. 增加未跟踪文件
	_ = os.WriteFile(filepath.Join(repo, "new_file.txt"), []byte("new untracked\n"), 0644)
	info, err = InspectRepo(repo)
	if err != nil {
		t.Fatalf("InspectRepo untracked: %v", err)
	}
	if !info.IsDirty || info.UntrackedNum != 1 {
		t.Errorf("expected dirty with 1 untracked, got: %+v", info)
	}
}

func TestCreateAndApplyDiffPayload(t *testing.T) {
	localRepo := initTestGitRepo(t)

	// 模拟 remoteRepo 作为 localRepo 的 clone
	remoteRepo := t.TempDir()
	ctx := context.Background()
	if _, err := runGit(ctx, remoteRepo, "clone", localRepo, "."); err != nil {
		t.Fatalf("clone to remote: %v", err)
	}
	_, _ = runGit(ctx, remoteRepo, "config", "user.email", "test@relay.dev")
	_, _ = runGit(ctx, remoteRepo, "config", "user.name", "Relay Tester")

	// 本地修改已跟踪文件 + 创建未跟踪文件
	_ = os.WriteFile(filepath.Join(localRepo, "README.md"), []byte("# Hello\nUpdated content by local!\n"), 0644)
	_ = os.WriteFile(filepath.Join(localRepo, "new_code.go"), []byte("package main\nfunc main() {}\n"), 0644)

	// 提取 Payload
	payload, err := CreateDiffPayload(localRepo, false)
	if err != nil {
		t.Fatalf("CreateDiffPayload: %v", err)
	}
	if len(payload.Patch) == 0 {
		t.Errorf("expected non-empty patch")
	}
	if len(payload.UntrackedArchive) == 0 {
		t.Errorf("expected non-empty untracked archive")
	}

	// 远端应用 Payload
	res, err := ApplyDiffPayload(remoteRepo, payload)
	if err != nil {
		t.Fatalf("ApplyDiffPayload: %v", err)
	}
	if !res.Applied {
		t.Errorf("expected res.Applied=true")
	}

	// 校验远端文件内容是否已同步
	readmeBytes, _ := os.ReadFile(filepath.Join(remoteRepo, "README.md"))
	if string(readmeBytes) != "# Hello\nUpdated content by local!\n" {
		t.Errorf("remote README content not matching: %s", string(readmeBytes))
	}
	codeBytes, _ := os.ReadFile(filepath.Join(remoteRepo, "new_code.go"))
	if string(codeBytes) != "package main\nfunc main() {}\n" {
		t.Errorf("remote new_code.go not found or not matching: %s", string(codeBytes))
	}
}

func TestZipSlipSecurityProtection(t *testing.T) {
	dest := t.TempDir()

	// 构造一个包含恶意 ../../ 路径的 tar.gz
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	maliciousHeader := &tar.Header{
		Name:     "../../etc/passwd",
		Mode:     0644,
		Size:     10,
		Typeflag: tar.TypeReg,
	}
	_ = tw.WriteHeader(maliciousHeader)
	_, _ = tw.Write([]byte("malicious!"))
	_ = tw.Close()
	_ = gw.Close()

	err := extractUntrackedArchive(dest, buf.Bytes())
	if err == nil {
		t.Fatalf("expected Zip Slip to be blocked, but succeeded")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("security violation")) {
		t.Errorf("expected security violation error, got: %v", err)
	}
}

func TestGitMetadataProtection(t *testing.T) {
	dest := t.TempDir()

	// 构造一个试图写入 .git/hooks/ 的恶意包
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	maliciousHeader := &tar.Header{
		Name:     ".git/hooks/post-commit",
		Mode:     0755,
		Size:     12,
		Typeflag: tar.TypeReg,
	}
	_ = tw.WriteHeader(maliciousHeader)
	_, _ = tw.Write([]byte("#!/bin/sh\nls"))
	_ = tw.Close()
	_ = gw.Close()

	err := extractUntrackedArchive(dest, buf.Bytes())
	if err == nil {
		t.Fatalf("expected .git write to be blocked, but succeeded")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("git metadata")) {
		t.Errorf("expected git metadata error, got: %v", err)
	}
}

func TestShadowSnapshotOnDirtyRemote(t *testing.T) {
	localRepo := initTestGitRepo(t)
	remoteRepo := t.TempDir()
	ctx := context.Background()
	_, _ = runGit(ctx, remoteRepo, "clone", localRepo, ".")
	_, _ = runGit(ctx, remoteRepo, "config", "user.email", "test@relay.dev")
	_, _ = runGit(ctx, remoteRepo, "config", "user.name", "Relay Tester")

	// 远端先行做出修改 (dirty 状态)
	_ = os.WriteFile(filepath.Join(remoteRepo, "README.md"), []byte("# Hello\nRemote modified line\n"), 0644)

	// 本地也做了修改
	_ = os.WriteFile(filepath.Join(localRepo, "README.md"), []byte("# Hello\nLocal modified line\n"), 0644)
	payload, err := CreateDiffPayload(localRepo, false)
	if err != nil {
		t.Fatalf("CreateDiffPayload: %v", err)
	}

	// 远端应用应当触发影子快照，并成功应用本地修改
	res, err := ApplyDiffPayload(remoteRepo, payload)
	if err != nil {
		t.Fatalf("ApplyDiffPayload: %v", err)
	}
	if res == nil || res.ShadowSnapshot == "" {
		t.Errorf("expected shadow snapshot to be created for dirty remote")
	}

	// 验证远端影子快照 ref 是否存在
	refOut, err := runGit(ctx, remoteRepo, "rev-parse", res.ShadowSnapshot)
	if err != nil || refOut == "" {
		t.Errorf("expected valid snapshot ref %s, got err: %v", res.ShadowSnapshot, err)
	}

	// 验证远端当前内容已经被同步为本地内容
	content, _ := os.ReadFile(filepath.Join(remoteRepo, "README.md"))
	if string(content) != "# Hello\nLocal modified line\n" {
		t.Errorf("expected local content, got: %s", string(content))
	}
}

func TestBaseCommitDrift_Blocked(t *testing.T) {
	localRepo := initTestGitRepo(t)
	remoteRepo := t.TempDir()
	ctx := context.Background()
	_, _ = runGit(ctx, remoteRepo, "clone", localRepo, ".")
	_, _ = runGit(ctx, remoteRepo, "config", "user.email", "test@relay.dev")
	_, _ = runGit(ctx, remoteRepo, "config", "user.name", "Relay Tester")

	// 远端产生了一个新提交，导致两端 base commit 分叉
	_ = os.WriteFile(filepath.Join(remoteRepo, "remote_only.txt"), []byte("diverged\n"), 0644)
	_, _ = runGit(ctx, remoteRepo, "add", "remote_only.txt")
	_, _ = runGit(ctx, remoteRepo, "commit", "-m", "remote commit")

	// 本地基于原有 commit 产生 diff
	_ = os.WriteFile(filepath.Join(localRepo, "README.md"), []byte("# Hello\nLocal modified\n"), 0644)
	payload, err := CreateDiffPayload(localRepo, false)
	if err != nil {
		t.Fatalf("CreateDiffPayload: %v", err)
	}

	// 默认未开启 force，应当报 base commit drift 拦截
	_, err = ApplyDiffPayload(remoteRepo, payload)
	if err == nil {
		t.Fatalf("expected base commit drift error, but succeeded")
	}
	if !strings.Contains(err.Error(), "base commit drift") {
		t.Errorf("expected drift error, got: %v", err)
	}

	// 开启 Force 应当允许覆盖
	payload.Force = true
	res, err := ApplyDiffPayload(remoteRepo, payload)
	if err != nil {
		t.Fatalf("expected force to succeed, got: %v", err)
	}
	if !res.Applied {
		t.Errorf("expected res.Applied=true")
	}
}
