package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/user/relay/internal/relay/protocol"
)

// ListSnapshots 查询指定工作区中所有可恢复的影子快照，按时间倒序排列
func ListSnapshots(dir string) ([]protocol.GitSnapshotInfo, error) {
	mu := getRepoLock(dir)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout)
	defer cancel()

	info, err := InspectRepo(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect repo: %w", err)
	}
	if !info.IsGitRepo {
		return nil, fmt.Errorf("%q is not a git repository", dir)
	}

	// 格式化输出: refname | commit_sha | subject | unix_timestamp
	format := "%(refname)|%(objectname)|%(contents:subject)|%(creatordate:unix)"
	out, err := runGit(ctx, dir, "for-each-ref", "--sort=-creatordate", "--format="+format, "refs/relay/backups/")
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var snapshots []protocol.GitSnapshotInfo
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		parts := strings.SplitN(l, "|", 4)
		if len(parts) < 3 {
			continue
		}
		ref := parts[0]
		sha := parts[1]
		msg := parts[2]
		var ts int64
		if len(parts) >= 4 {
			ts, _ = strconv.ParseInt(parts[3], 10, 64)
		}
		// 若无法从 git 属性解析时间戳，尝试从 ref 名 refs/relay/backups/<timestamp> 提取
		if ts == 0 {
			refParts := strings.Split(ref, "/")
			if len(refParts) > 0 {
				ts, _ = strconv.ParseInt(refParts[len(refParts)-1], 10, 64)
			}
		}

		snapshots = append(snapshots, protocol.GitSnapshotInfo{
			Ref:       ref,
			CommitSHA: sha,
			Message:   msg,
			Timestamp: ts,
		})
	}

	return snapshots, nil
}

// RestoreSnapshot 恢复指定影子快照到工作区。若 targetRef 为空，恢复最新的快照。
func RestoreSnapshot(dir string, targetRef string) (*protocol.GitUndoResponse, error) {
	mu := getRepoLock(dir)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout*2)
	defer cancel()

	info, err := InspectRepo(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect repo: %w", err)
	}
	if !info.IsGitRepo {
		return nil, fmt.Errorf("%q is not a git repository", dir)
	}

	// 1. 若未指定 targetRef，查找最新快照
	format := "%(refname)|%(objectname)"
	out, err := runGit(ctx, dir, "for-each-ref", "--sort=-creatordate", "--format="+format, "refs/relay/backups/")
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("no backup snapshot found to restore in %s", dir)
	}

	var chosenRef, chosenSHA string
	if targetRef == "" {
		parts := strings.Split(lines[0], "|")
		chosenRef = parts[0]
		if len(parts) > 1 {
			chosenSHA = parts[1]
		}
	} else {
		for _, l := range lines {
			parts := strings.Split(l, "|")
			if parts[0] == targetRef || strings.HasSuffix(parts[0], "/"+targetRef) {
				chosenRef = parts[0]
				if len(parts) > 1 {
					chosenSHA = parts[1]
				}
				break
			}
		}
		if chosenRef == "" {
			return nil, fmt.Errorf("snapshot %q not found", targetRef)
		}
	}

	resp := &protocol.GitUndoResponse{
		RestoredRef: chosenRef,
	}

	// 2. 状态保护: 若当前工作区有 dirty 改动，先创建一个安全快照防止误恢复导致的数据永久丢失
	if info.IsDirty {
		safetyRef := fmt.Sprintf("refs/relay/backups/safety-%d", time.Now().Unix())
		tag := fmt.Sprintf("relay-safety-pre-undo-%d", time.Now().UnixNano())
		_, _ = runGit(ctx, dir, "stash", "push", "-u", "-m", tag)
		if stashCommit, rerr := runGit(ctx, dir, "rev-parse", "refs/stash"); rerr == nil && stashCommit != "" {
			_, _ = runGit(ctx, dir, "update-ref", safetyRef, stashCommit)
			resp.SafetyRef = safetyRef
		}
	}

	// 3. 清理当前工作区以便干净恢复
	_, _ = runGit(ctx, dir, "reset", "--hard", "HEAD")
	_, _ = runGit(ctx, dir, "clean", "-fd")

	// 4. 应用目标快照
	// git stash apply 接受 commit SHA 或 ref
	applyTarget := chosenSHA
	if applyTarget == "" {
		applyTarget = chosenRef
	}

	applyOut, applyErr := runGit(ctx, dir, "stash", "apply", applyTarget)
	if applyErr != nil {
		return nil, fmt.Errorf("apply snapshot %s failed: %w (output: %s)", chosenRef, applyErr, applyOut)
	}

	resp.OK = true
	return resp, nil
}
