package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUndo_ListAndRestore(t *testing.T) {
	dir := initTestGitRepo(t)

	// 1. 空快照列表测试
	snapshots, err := ListSnapshots(dir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("expected 0 snapshots, got %d", len(snapshots))
	}

	// 2. 模拟本地改动并产生快照
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v2-modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked-v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	refName, err := createShadowSnapshot(ctx, dir)
	if err != nil {
		t.Fatalf("createShadowSnapshot: %v", err)
	}
	if !strings.HasPrefix(refName, "refs/relay/backups/") {
		t.Fatalf("unexpected snapshot ref: %q", refName)
	}

	// 3. 查询快照列表
	snapshots, err = ListSnapshots(dir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Ref != refName {
		t.Errorf("expected ref %s, got %s", refName, snapshots[0].Ref)
	}

	// 4. 工作区发生误改（产生错误修改和新文件）
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("corrupted-v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 5. 执行 RestoreSnapshot 还原
	resp, err := RestoreSnapshot(dir, "")
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !resp.OK {
		t.Fatalf("RestoreSnapshot response not OK")
	}
	if resp.RestoredRef != refName {
		t.Errorf("RestoredRef = %q, want %q", resp.RestoredRef, refName)
	}
	if resp.SafetyRef == "" {
		t.Errorf("SafetyRef should be created when restoring dirty workspace")
	}

	// 6. 验证工作区是否成功恢复回 v2 状态
	fileContent, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(fileContent) != "v2-modified" {
		t.Errorf("file.txt content = %q, want %q", string(fileContent), "v2-modified")
	}

	// 验证垃圾文件已被清除
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Errorf("junk.txt should have been cleaned up")
	}
}
