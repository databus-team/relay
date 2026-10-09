package supervisor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSupervisor_StartAndListTask(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("echo 'hello supervisor test'", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	if task.Meta().ID == "" {
		t.Errorf("task ID should not be empty")
	}

	// 等待任务完成
	time.Sleep(300 * time.Millisecond)

	meta := task.Meta()
	if meta.Status != StatusExited {
		t.Errorf("expected status %s, got %s", StatusExited, meta.Status)
	}
	if meta.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", meta.ExitCode)
	}

	// 验证日志落盘
	logBytes, err := os.ReadFile(meta.LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), "hello supervisor test") {
		t.Errorf("log output missing expected string, got: %s", string(logBytes))
	}

	// ListTasks
	allTasks := sup.ListTasks(true)
	if len(allTasks) != 1 {
		t.Errorf("expected 1 task in list, got %d", len(allTasks))
	}

	runningTasks := sup.ListTasks(false)
	if len(runningTasks) != 0 {
		t.Errorf("expected 0 running tasks, got %d", len(runningTasks))
	}
}

func TestSupervisor_StopTask(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("sleep 10", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	if task.Meta().Status != StatusRunning {
		t.Errorf("expected running, got %s", task.Meta().Status)
	}

	err = sup.StopTask(task.Meta().ID)
	if err != nil {
		t.Fatalf("stop task: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	meta := task.Meta()
	if meta.Status != StatusStopped {
		t.Errorf("expected stopped status, got %s", meta.Status)
	}
	if isProcessAlive(meta.PID) {
		t.Errorf("process %d should not be alive after stop", meta.PID)
	}
}

func TestSupervisor_StreamLogsFollow(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("printf 'line1\n'; sleep 0.2; printf 'line2\n'", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var collected []string
	err = sup.StreamLogs(ctx, task.Meta().ID, true, 10, func(chunk string) {
		collected = append(collected, chunk)
	})
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("stream logs: %v", err)
	}

	joined := strings.Join(collected, "")
	if !strings.Contains(joined, "line1") || !strings.Contains(joined, "line2") {
		t.Errorf("missing expected streamed lines, got: %s", joined)
	}
}

func TestSupervisor_PersistenceAndRecovery(t *testing.T) {
	dir := t.TempDir()
	sup1, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup1.StartTask("echo 'persistence test'", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// 重新初始化 Supervisor
	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}

	recovered, err := sup2.GetTask(task.Meta().ID)
	if err != nil {
		t.Fatalf("get recovered task: %v", err)
	}

	if recovered.Meta().Status != StatusExited {
		t.Errorf("expected recovered status %s, got %s", StatusExited, recovered.Meta().Status)
	}
}
