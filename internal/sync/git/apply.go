package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// repoLocks 为各目录提供进程内互斥锁，防止并发执行同一工作区的 git 同步
var (
	repoLocksMu sync.Mutex
	repoLocks   = make(map[string]*sync.Mutex)
)

func getRepoLock(dir string) *sync.Mutex {
	repoLocksMu.Lock()
	defer repoLocksMu.Unlock()
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	l, ok := repoLocks[dir]
	if !ok {
		l = &sync.Mutex{}
		repoLocks[dir] = l
	}
	return l
}

// ApplyResult 描述应用补丁后的结果回执
type ApplyResult struct {
	Applied        bool
	ShadowSnapshot string // 若触发了影子快照，记录其 ref 名字
}

// ApplyDiffPayload 远端安全接收并应用 DiffPayload
func ApplyDiffPayload(dir string, payload *DiffPayload) (*ApplyResult, error) {
	if payload == nil {
		return &ApplyResult{Applied: true}, nil
	}

	mu := getRepoLock(dir)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout*3)
	defer cancel()

	info, err := InspectRepo(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect remote repo: %w", err)
	}
	if !info.IsGitRepo {
		return nil, fmt.Errorf("%q is not a git repository on target node", dir)
	}

	// 1. 基准 Commit SHA 比对 (防基线漂移损坏代码)
	if payload.BaseCommit != "" && info.BaseCommit != "" && payload.BaseCommit != info.BaseCommit {
		if !payload.Force {
			return nil, fmt.Errorf("base commit drift detected: remote is at %s, but local expects %s. Align commits or use --force-remote",
				ShortSHA(info.BaseCommit), ShortSHA(payload.BaseCommit))
		}
	}

	res := &ApplyResult{}

	// 2. 远端影子快照保护 (Zero Data Loss)
	if info.IsDirty {
		snapshotRef, err := createShadowSnapshot(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("create shadow snapshot: %w", err)
		}
		res.ShadowSnapshot = snapshotRef
	}

	// 3. 若指定 Force: 先清理远端冲突残留工作区
	if payload.Force {
		_, _ = runGit(ctx, dir, "reset", "--hard", "HEAD")
	}

	// 4. 应用 Patch
	if len(payload.Patch) > 0 {
		// 预检 git apply --check (CRLF 与空格宽容)
		checkCmd := []string{"apply", "--check", "--ignore-whitespace", "--whitespace=nowarn"}
		if err := runGitWithStdin(ctx, dir, payload.Patch, checkCmd...); err != nil {
			if !payload.Force {
				return res, fmt.Errorf("git apply conflict: %w (use --force-remote to override)", err)
			}
			// 强行清理后再试
			_, _ = runGit(ctx, dir, "reset", "--hard", "HEAD")
			_, _ = runGit(ctx, dir, "clean", "-fd")
		}

		// 正式应用
		applyCmd := []string{"apply", "--ignore-whitespace", "--whitespace=nowarn"}
		if err := runGitWithStdin(ctx, dir, payload.Patch, applyCmd...); err != nil {
			return nil, fmt.Errorf("git apply failed: %w", err)
		}
	}

	// 5. 解压未跟踪文件 (严格防 Zip Slip 路径穿越与禁止触碰 .git 目录)
	if len(payload.UntrackedArchive) > 0 {
		if err := extractUntrackedArchive(dir, payload.UntrackedArchive); err != nil {
			return nil, fmt.Errorf("unpack untracked files: %w", err)
		}
	}

	res.Applied = true
	return res, nil
}

// createShadowSnapshot 创建影子快照并保存为 refs/relay/backups/<timestamp>
func createShadowSnapshot(ctx context.Context, dir string) (string, error) {
	tag := fmt.Sprintf("relay-shadow-%d", time.Now().UnixNano())
	refName := fmt.Sprintf("refs/relay/backups/%d", time.Now().Unix())

	// git stash push -u: 同时保留 tracked 和 untracked 文件
	_, err := runGit(ctx, dir, "stash", "push", "-u", "-m", tag)
	if err != nil {
		return "", err
	}

	// 获取最新的 stash commit
	stashCommit, err := runGit(ctx, dir, "rev-parse", "refs/stash")
	if err == nil && stashCommit != "" {
		// 写入影子引用
		_, _ = runGit(ctx, dir, "update-ref", refName, stashCommit)
	}

	return refName, nil
}

// extractUntrackedArchive 解压未跟踪文件包，强制实施严密的安全校验
func extractUntrackedArchive(destDir string, archiveData []byte) error {
	gr, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// 规范化相对路径
		cleanName := filepath.ToSlash(filepath.Clean(hdr.Name))

		// 安全拦截 1: 绝对禁止包含 ..
		if strings.HasPrefix(cleanName, "../") || cleanName == ".." {
			return fmt.Errorf("security violation: directory traversal in archive path %q", hdr.Name)
		}

		// 安全拦截 2: 绝对禁止写入 .git/ 内部目录或任何同名文件
		if strings.HasPrefix(cleanName, ".git/") || cleanName == ".git" {
			return fmt.Errorf("security violation: attempted write to git metadata %q", hdr.Name)
		}

		targetPath := filepath.Join(absDest, filepath.FromSlash(cleanName))

		// 安全拦截 3: 严格校验目标路径必须在 destDir 内部 (Zip Slip 校验)
		if !strings.HasPrefix(targetPath, absDest+string(filepath.Separator)) && targetPath != absDest {
			return fmt.Errorf("security violation: extracted path %q escapes root %q", targetPath, absDest)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode&0755)|0644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// runGitWithStdin 向 git 进程注入 stdin 并执行
func runGitWithStdin(ctx context.Context, dir string, stdinData []byte, args ...string) error {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	c.Stdin = bytes.NewReader(stdinData)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "PAGER=cat", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ShortSHA 截取 commit SHA 前 8 位作为简短标识
func ShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(empty)"
	}
	return sha
}
