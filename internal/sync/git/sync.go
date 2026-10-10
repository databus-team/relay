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
	"time"
)

const (
	// MaxSingleFileSize 是单个未跟踪文件打包的最大体积 (10MB)
	MaxSingleFileSize = 10 * 1024 * 1024
	// MaxTotalArchiveSize 是未跟踪文件归档包的最大体积 (50MB)
	MaxTotalArchiveSize = 50 * 1024 * 1024
	// CommandTimeout 单次 Git 命令超时
	CommandTimeout = 15 * time.Second
)

// DiffPayload 描述执行前同步的工作区增量变更载荷
type DiffPayload struct {
	BaseCommit       string `json:"base_commit"`
	Patch            []byte `json:"patch"`             // git diff --binary HEAD 文本
	UntrackedArchive []byte `json:"untracked_archive"` // tar.gz 压缩的未跟踪文件
	Force            bool   `json:"force,omitempty"`   // 是否强制覆盖远端
}

// StatusInfo 描述当前工作区的 Git 状态简报
type StatusInfo struct {
	IsGitRepo      bool
	CurrentBranch  string
	BaseCommit     string
	IsDirty        bool
	UntrackedNum   int
	DirtyFiles     []string
	UntrackedFiles []string
}

// runGitBytes 在指定目录下执行 git 命令并返回原始 stdout 字节
func runGitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "PAGER=cat", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s failed: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// runGit 在指定目录下执行 git 命令并返回去除首尾空白的 string
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitBytes(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// InspectRepo 探测目录是否为合法 Git 仓库，并返回其 HEAD Commit 与脏状态
func InspectRepo(dir string) (StatusInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout)
	defer cancel()

	// 1. 验证是否为 git 仓库
	isRepo, err := runGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || isRepo != "true" {
		return StatusInfo{IsGitRepo: false}, nil
	}

	// 2. 获取当前 HEAD commit SHA 与当前分支
	commit, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		// 可能是全新的空仓库 (尚无 commit)
		commit = ""
	}
	branch, _ := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")

	// 3. 检查状态: porcelain 输出
	statusOut, err := runGit(ctx, dir, "status", "--porcelain")
	if err != nil {
		return StatusInfo{IsGitRepo: true, BaseCommit: commit, CurrentBranch: branch}, err
	}

	lines := strings.Split(statusOut, "\n")
	untrackedCount := 0
	dirty := false
	var dirtyFiles []string
	var untrackedFiles []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		dirty = true
		if strings.HasPrefix(l, "??") {
			untrackedCount++
			untrackedFiles = append(untrackedFiles, strings.TrimSpace(strings.TrimPrefix(l, "??")))
		} else if len(l) > 2 {
			dirtyFiles = append(dirtyFiles, strings.TrimSpace(l[2:]))
		}
	}

	return StatusInfo{
		IsGitRepo:      true,
		CurrentBranch:  branch,
		BaseCommit:     commit,
		IsDirty:        dirty,
		UntrackedNum:   untrackedCount,
		DirtyFiles:     dirtyFiles,
		UntrackedFiles: untrackedFiles,
	}, nil
}

// CreateDiffPayload 提取本地未提交的修改 (diff --binary HEAD) 与白名单内的未跟踪小文件
func CreateDiffPayload(dir string, force bool) (*DiffPayload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout*2)
	defer cancel()

	info, err := InspectRepo(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect repo: %w", err)
	}
	if !info.IsGitRepo {
		return nil, fmt.Errorf("%q is not a git repository", dir)
	}

	// 1. 提取跟踪文件的修改 diff
	patch := []byte{}
	if info.BaseCommit != "" {
		patchRaw, err := runGitBytes(ctx, dir, "diff", "--binary", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("extract diff: %w", err)
		}
		patch = patchRaw
	}

	// 2. 查找未跟踪文件 (复用 InspectRepo 已解析出的结果，处理引号)
	untrackedFiles := make([]string, 0, len(info.UntrackedFiles))
	for _, f := range info.UntrackedFiles {
		clean := strings.Trim(strings.TrimSpace(f), "\"")
		if clean != "" {
			untrackedFiles = append(untrackedFiles, clean)
		}
	}

	// 3. 打包未跟踪文件为 tar.gz 内存流 (严格限制体积与排除安全高危文件)
	var archiveBuf bytes.Buffer
	if len(untrackedFiles) > 0 {
		gw := gzip.NewWriter(&archiveBuf)
		tw := tar.NewWriter(gw)
		var totalSize int64

		for _, rel := range untrackedFiles {
			fullPath := filepath.Join(dir, rel)
			fi, err := os.Lstat(fullPath)
			if err != nil {
				continue
			}

			// 忽略目录自身的递归遍历，只写文件
			if fi.IsDir() {
				// 遍历子文件
				err := filepath.Walk(fullPath, func(subPath string, subFi os.FileInfo, walkErr error) error {
					if walkErr != nil || subFi.IsDir() {
						return walkErr
					}
					subRel, err := filepath.Rel(dir, subPath)
					if err != nil {
						return nil
					}
					return archiveSingleFile(tw, dir, subRel, subFi, &totalSize)
				})
				if err != nil {
					return nil, err
				}
				continue
			}

			if err := archiveSingleFile(tw, dir, rel, fi, &totalSize); err != nil {
				return nil, err
			}
		}

		if err := tw.Close(); err != nil {
			return nil, err
		}
		if err := gw.Close(); err != nil {
			return nil, err
		}
	}

	return &DiffPayload{
		BaseCommit:       info.BaseCommit,
		Patch:            patch,
		UntrackedArchive: archiveBuf.Bytes(),
		Force:            force,
	}, nil
}

// archiveSingleFile 向 tar 打包器写入单个文件，做防路径穿越与体积限制
func archiveSingleFile(tw *tar.Writer, baseDir, rel string, fi os.FileInfo, totalSize *int64) error {
	// 安全防范: 严禁将 .git/ 目录下的文件打入包
	cleanRel := filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(cleanRel, ".git/") || cleanRel == ".git" {
		return nil
	}
	if strings.HasPrefix(cleanRel, "../") || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid untracked file path: %s", rel)
	}

	// 忽略软链接指向外部
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	if fi.Size() > MaxSingleFileSize {
		return fmt.Errorf("untracked file %s exceeds max single size (%d MB)", rel, MaxSingleFileSize/(1024*1024))
	}
	*totalSize += fi.Size()
	if *totalSize > MaxTotalArchiveSize {
		return fmt.Errorf("total untracked archive exceeds max limit (%d MB)", MaxTotalArchiveSize/(1024*1024))
	}

	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	hdr.Name = cleanRel

	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	f, err := os.Open(filepath.Join(baseDir, rel))
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tw, f)
	return err
}
