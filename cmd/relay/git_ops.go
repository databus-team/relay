package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/user/relay/internal/config"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/sync/git"
)

func resolveGitTarget(cfg *config.Config, nodeFlag, execFlag, watchFlag string) (string, string, string, error) {
	workspaceID := watchFlag
	if workspaceID == "" {
		if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
			workspaceID = inferred
		}
	}

	targetExec := resolveNodeTarget(nodeFlag, execFlag)
	localDir := "."

	if workspaceID != "" {
		if ws, err := cfg.GetWorkspaceByID(workspaceID); err == nil {
			if targetExec == "" {
				targetExec = ws.Executor
			}
			if ws.LocalDir != "" {
				localDir = ws.LocalDir
			}
		}
	}

	if targetExec == "" {
		return "", "", "", fmt.Errorf("target node required (-e/--node/--executor or bind to a workspace via -w)")
	}

	return targetExec, workspaceID, localDir, nil
}

func getRelayBackend(cfg *config.Config) (*relaybackend.RelayBackend, error) {
	if cfg.Backend.Type != "relay" {
		return nil, fmt.Errorf("command requires backend.type=relay, got %q", cfg.Backend.Type)
	}
	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		return nil, fmt.Errorf("create backend: %w", err)
	}
	rb, ok := b.(*relaybackend.RelayBackend)
	if !ok {
		return nil, fmt.Errorf("backend does not implement relay backend")
	}
	return rb, nil
}

// runUndo 恢复远端工作区影子快照或列出所有可用快照
func runUndo() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	targetExec, workspaceID, _, err := resolveGitTarget(cfg, *undoNode, *undoExec, *undoWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	rb, err := getRelayBackend(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer rb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. 列出快照
	if *undoList {
		statusResp, err := rb.GitStatus(ctx, targetExec, workspaceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to query snapshots from %s: %v\n", targetExec, err)
			os.Exit(1)
		}
		if !statusResp.OK {
			fmt.Fprintf(os.Stderr, "Error from %s: %s\n", targetExec, statusResp.Error)
			os.Exit(1)
		}

		if len(statusResp.Snapshots) == 0 {
			fmt.Printf("No shadow snapshots found on node %s (workspace: %s)\n", targetExec, workspaceID)
			return
		}

		fmt.Printf("Shadow snapshots on node %s (workspace: %s):\n\n", targetExec, workspaceID)
		fmt.Printf("%-32s %-10s %-20s %s\n", "SNAPSHOT REF", "COMMIT", "TIME", "MESSAGE")
		for _, s := range statusResp.Snapshots {
			tStr := "-"
			if s.Timestamp > 0 {
				tStr = time.Unix(s.Timestamp, 0).Format("2006-01-02 15:04:05")
			}
			cShort := s.CommitSHA
			if len(cShort) > 8 {
				cShort = cShort[:8]
			}
			fmt.Printf("%-32s %-10s %-20s %s\n", s.Ref, cShort, tStr, s.Message)
		}
		return
	}

	// 2. 还原快照
	fmt.Printf("Restoring remote workspace on %s (workspace: %s)...\n", targetExec, workspaceID)
	undoResp, err := rb.GitUndo(ctx, targetExec, workspaceID, *undoSnapshot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Undo failed: %v\n", err)
		os.Exit(1)
	}
	if !undoResp.OK {
		fmt.Fprintf(os.Stderr, "Undo failed on %s: %s\n", targetExec, undoResp.Error)
		os.Exit(1)
	}

	fmt.Printf("✔ Successfully restored remote workspace to snapshot: %s\n", undoResp.RestoredRef)
	if undoResp.SafetyRef != "" {
		fmt.Printf("ℹ Safety notice: pre-undo dirty changes preserved in: %s\n", undoResp.SafetyRef)
	}
}

// runDiff 对比本地工作区与远端工作区的 Git 状态及冲突预警
func runDiff() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	targetExec, workspaceID, localDir, err := resolveGitTarget(cfg, *diffNode, *diffExec, *diffWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	rb, err := getRelayBackend(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer rb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. 本地状态检查
	absLocal, _ := filepath.Abs(localDir)
	localInfo, err := git.InspectRepo(absLocal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Inspect local repo %s failed: %v\n", absLocal, err)
		os.Exit(1)
	}

	// 2. 远端状态检查
	remoteResp, err := rb.GitStatus(ctx, targetExec, workspaceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Query remote Git status from %s failed: %v\n", targetExec, err)
		os.Exit(1)
	}
	if !remoteResp.OK {
		fmt.Fprintf(os.Stderr, "Remote Git query error: %s\n", remoteResp.Error)
		os.Exit(1)
	}

	// 3. 结构化打印比对
	fmt.Println("================================================================================")
	fmt.Printf(" Workspace Sync Status & Drift Analysis\n")
	fmt.Printf(" Local:  %s\n", absLocal)
	fmt.Printf(" Remote: %s (workspace: %s)\n", targetExec, workspaceID)
	fmt.Println("================================================================================")

	// 基准提交比对
	localBaseShort := git.ShortSHA(localInfo.BaseCommit)
	remoteBaseShort := git.ShortSHA(remoteResp.BaseCommit)
	if localInfo.BaseCommit == remoteResp.BaseCommit {
		fmt.Printf("✓ Base Commit:       Aligned at %s\n", localBaseShort)
	} else {
		fmt.Printf("⚠ Base Commit Drift: Local is at %s, but Remote is at %s\n", localBaseShort, remoteBaseShort)
		fmt.Printf("                     Hint: Sync commits or pass --force-remote to override\n")
	}

	// 本地改动概览
	fmt.Printf("\n[Local Workspace Status]:\n")
	if !localInfo.IsDirty {
		fmt.Println("  Clean (no uncommitted changes)")
	} else {
		fmt.Printf("  Dirty: %d modified/staged, %d untracked\n", len(localInfo.DirtyFiles), len(localInfo.UntrackedFiles))
		for _, f := range localInfo.DirtyFiles {
			fmt.Printf("    * modified: %s\n", f)
		}
		for _, f := range localInfo.UntrackedFiles {
			fmt.Printf("    + untracked: %s\n", f)
		}
	}

	// 远端改动概览
	fmt.Printf("\n[Remote Workspace Status on %s]:\n", targetExec)
	if !remoteResp.IsDirty {
		fmt.Println("  Clean (no uncommitted remote changes)")
	} else {
		fmt.Printf("  Dirty: %d modified/staged, %d untracked\n", len(remoteResp.DirtyFiles), len(remoteResp.UntrackedFiles))
		for _, f := range remoteResp.DirtyFiles {
			fmt.Printf("    * modified: %s\n", f)
		}
		for _, f := range remoteResp.UntrackedFiles {
			fmt.Printf("    + untracked: %s\n", f)
		}
	}

	// 冲突预警检测
	remoteModifiedSet := make(map[string]bool)
	for _, f := range remoteResp.DirtyFiles {
		remoteModifiedSet[f] = true
	}
	for _, f := range remoteResp.UntrackedFiles {
		remoteModifiedSet[f] = true
	}

	var conflicts []string
	for _, f := range localInfo.DirtyFiles {
		if remoteModifiedSet[f] {
			conflicts = append(conflicts, f)
		}
	}
	for _, f := range localInfo.UntrackedFiles {
		if remoteModifiedSet[f] {
			conflicts = append(conflicts, f)
		}
	}

	fmt.Println("\n--------------------------------------------------------------------------------")
	if len(conflicts) > 0 {
		fmt.Printf("⚡ CONFLICT WARNING: %d file(s) modified on BOTH local and remote:\n", len(conflicts))
		for _, c := range conflicts {
			fmt.Printf("   ! %s\n", c)
		}
		fmt.Println("   Syncing will overwrite remote changes (a shadow snapshot will be created).")
		fmt.Println("   To undo remote overwrite after exec, use: relay undo")
	} else {
		fmt.Println("✓ No direct file conflicts detected between local and remote.")
	}

	if len(remoteResp.Snapshots) > 0 {
		fmt.Printf("✓ %d shadow snapshot(s) available on remote for recovery (relay undo --list)\n", len(remoteResp.Snapshots))
	}
	fmt.Println("================================================================================")
}
