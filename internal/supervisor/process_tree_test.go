//go:build !windows

package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTaskStopKillsDescendants 钉住「stop 终止的是整棵进程树,不只是 shell」这条契约。
// shell 用 `&` 显式 fork 出子进程并 wait,只杀 shell 时子进程会继续跑(Windows 上曾因
// taskkill /T 枚举不到 MSYS fork 出来的进程而真实逃逸);unix 靠进程组覆盖后代。
//
// 只在 unix 跑:子进程 pid 取自 shell 的 $!,那是 MSYS 自己的 pid,Windows 的 isProcessAlive
// 按 Windows pid 查 tasklist,断言不出正确结果(Windows 侧的 job 由实机验证覆盖)。
func TestTaskStopKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	task, err := sup.StartTask("yes > /dev/null & echo $! > "+pidFile+" ; wait", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	childPID := waitChildPID(t, pidFile)
	if !isProcessAlive(childPID) {
		t.Fatalf("child %d should be running before stop", childPID)
	}

	if err := sup.StopTask(task.Meta().ID); err != nil {
		t.Fatalf("stop task: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for isProcessAlive(childPID) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived stop (shell pid %d)", childPID, task.Meta().PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitChildPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child pid file %s never appeared", pidFile)
	return 0
}
