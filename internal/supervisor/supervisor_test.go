package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func waitTaskStatus(t *testing.T, task *Task, want TaskStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if task.Meta().Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach status %s (current %s)", task.Meta().ID, want, task.Meta().Status)
}

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

func TestSupervisor_StopAll(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task1, err := sup.StartTask("sleep 10", "", nil)
	if err != nil {
		t.Fatalf("start task 1: %v", err)
	}
	task2, err := sup.StartTask("sleep 10", "", nil)
	if err != nil {
		t.Fatalf("start task 2: %v", err)
	}

	sup.StopAll()

	time.Sleep(200 * time.Millisecond)

	for _, task := range []*Task{task1, task2} {
		meta := task.Meta()
		if meta.Status != StatusStopped {
			t.Errorf("task %s expected stopped, got %s", meta.ID, meta.Status)
		}
		if isProcessAlive(meta.PID) {
			t.Errorf("process %d should be terminated", meta.PID)
		}
	}
}

// TestSupervisor_RestartExitedTaskKeepsIdentity 覆盖「对已退出的任务重启：id 与目录不变，
// 状态回到运行态，Ports 保留」。命令首次运行 touch 标记后退出，重启时标记已存在则长跑，
// 让重启后的运行态可稳定观察。
func TestSupervisor_RestartExitedTaskKeepsIdentity(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	cwd := t.TempDir()
	marker := filepath.Join(cwd, "marker")
	cmdStr := fmt.Sprintf(`if [ -f %q ]; then sleep 5; else touch %q; fi`, marker, marker)
	ports := []string{"127.0.0.1:18080"}

	task, err := sup.StartTaskWithPorts(cmdStr, cwd, ports, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	waitTaskStatus(t, task, StatusExited, 5*time.Second)

	origID := task.Meta().ID
	origDir := task.dir

	restarted, err := sup.RestartTask(origID)
	if err != nil {
		t.Fatalf("restart task: %v", err)
	}
	defer func() { _ = sup.StopTask(origID) }()

	if got := restarted.Meta().ID; got != origID {
		t.Errorf("task id changed: want %s, got %s", origID, got)
	}
	if got := restarted.dir; got != origDir {
		t.Errorf("task dir changed: want %s, got %s", origDir, got)
	}
	if got := restarted.Meta().Status; got != StatusRunning {
		t.Errorf("expected running after restart, got %s", got)
	}
	if got := restarted.Meta().Ports; !reflect.DeepEqual(got, ports) {
		t.Errorf("ports not preserved: want %v, got %v", ports, got)
	}

	// 接线：同 id 重启必须宣告旧实例被取代。旧实例的收尾 goroutine 会在 cmd.Wait() 之后写回
	// 记录，而两个 Task 各持自己的锁、没有任何东西给这两次写排序；漏掉这一句，盘上留下的
	// 就可能是旧实例的 status/pid（见 TestSupervisor_SupersededTaskKeepsReplacementRecord）。
	task.mu.RLock()
	superseded := task.superseded
	task.mu.RUnlock()
	if !superseded {
		t.Errorf("RestartTask did not mark the previous instance superseded: its reaper can overwrite the replacement's record")
	}
}

// TestSupervisor_RestartRunningTaskReplacesProcess 覆盖「对正在运行的任务重启：旧进程组被
// 终止，新进程启动，只有一个实例在跑」。
func TestSupervisor_RestartRunningTaskReplacesProcess(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("sleep 5", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID
	oldPID := task.Meta().PID

	restarted, err := sup.RestartTask(id)
	if err != nil {
		t.Fatalf("restart task: %v", err)
	}
	defer func() { _ = sup.StopTask(id) }()

	if got := restarted.Meta().PID; got == oldPID {
		t.Errorf("restart reused the old pid %d", oldPID)
	}
	if got := restarted.Meta().Status; got != StatusRunning {
		t.Errorf("expected running after restart, got %s", got)
	}
	if isProcessAlive(oldPID) {
		t.Errorf("old process %d still alive after restart", oldPID)
	}
	if !isProcessAlive(restarted.Meta().PID) {
		t.Errorf("new process %d is not alive", restarted.Meta().PID)
	}
}

// TestSupervisor_SupersededTaskKeepsReplacementRecord 覆盖 A1:旧实例的收尾不得写回记录。
// 那个竞态的本来面目是旧实例收尾的 persist 与新实例的 persist 抢同一个 meta.json,而两个 Task
// 各持自己的锁、没人能排序。这里把时序钉死:进程还活着时先写下新实例的记录,再直接杀掉旧进程 ——
// 收尾 goroutine 只可能在 cmd.Wait() 返回之后写回,所以磁盘记录是否被覆盖,无歧义地说明收尾
// 有没有跳过持久化。真机上的竞态落点无法用普通重启可靠复现,故不对那一层做断言。
func TestSupervisor_SupersededTaskKeepsReplacementRecord(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("sleep 30", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID
	defer func() { _ = task.Stop() }()

	// 同 id 重启就是这样宣告旧对象被取代的。
	task.markSuperseded()

	// 新实例已经落盘(pid 与旧实例不同),而旧进程还活着:它的收尾还卡在 cmd.Wait()。
	sentinel := task.Meta()
	sentinel.PID = task.Meta().PID + 1
	data, err := json.MarshalIndent(sentinel, "", "  ")
	if err != nil {
		t.Fatalf("marshal replacement record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "meta.json"), data, 0o644); err != nil {
		t.Fatalf("write replacement record: %v", err)
	}

	_ = killProcessTree(task.Meta().PID)

	select {
	case <-task.done:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded task lifecycle goroutine did not finish")
	}

	got := readTaskMeta(t, dir, id)
	if got.PID != sentinel.PID || got.Status != StatusRunning {
		t.Errorf("superseded task wrote back its own record: pid %d status %s, want pid %d running",
			got.PID, got.Status, sentinel.PID)
	}

	// 重载该目录的 supervisor 必须读到替代实例的记录,而不是旧实例的收尾记录。
	reloaded, err := New(dir)
	if err != nil {
		t.Fatalf("reload supervisor: %v", err)
	}
	loaded, err := reloaded.GetTask(id)
	if err != nil {
		t.Fatalf("get reloaded task: %v", err)
	}
	if loaded.Meta().PID != sentinel.PID {
		t.Errorf("reloaded record pid = %d, want the replacement %d", loaded.Meta().PID, sentinel.PID)
	}
}

// TestSupervisor_RestartClaimedProcessAfterExecutorCrash 覆盖「对执行方崩溃后遗留的存活进程
// 重启：有界等待后新进程启动，调用不会无限阻塞，也不会同时跑两个实例」。
func TestSupervisor_RestartClaimedProcessAfterExecutorCrash(t *testing.T) {
	dir := t.TempDir()
	sup1, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 1: %v", err)
	}

	task, err := sup1.StartTask("sleep 5", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID
	oldPID := task.Meta().PID

	// 模拟执行方崩溃后重新拉起：新进程读到的记录里 running 且 PID 存活，但该进程不属于本进程。
	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	claimed, err := sup2.GetTask(id)
	if err != nil {
		t.Fatalf("get claimed task: %v", err)
	}
	if got := claimed.Meta().Status; got != StatusRunning {
		t.Fatalf("expected claimed task to read as running, got %s", got)
	}

	type restartResult struct {
		task *Task
		err  error
	}
	done := make(chan restartResult, 1)
	go func() {
		tk, err := sup2.RestartTask(id)
		done <- restartResult{task: tk, err: err}
	}()

	var res restartResult
	select {
	case res = <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("restart of a claimed process did not return within the bounded wait")
	}
	if res.err != nil {
		t.Fatalf("restart task: %v", res.err)
	}
	defer func() { _ = sup2.StopTask(id) }()

	if isProcessAlive(oldPID) {
		t.Errorf("leftover process %d still alive after restart", oldPID)
	}
	if !isProcessAlive(res.task.Meta().PID) {
		t.Errorf("new process %d is not alive", res.task.Meta().PID)
	}
}

// TestSupervisor_RestartPreservesCreatedAtUpdatesStartTime 覆盖「重启后首次创建时间不变，
// 最近一次启动时间被更新」。
func TestSupervisor_RestartPreservesCreatedAtUpdatesStartTime(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("sleep 0.2", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	waitTaskStatus(t, task, StatusExited, 5*time.Second)

	id := task.Meta().ID
	created := task.Meta().CreatedAt
	start := task.Meta().StartTime
	if created.IsZero() {
		t.Fatalf("created_at should be recorded at first creation")
	}

	restarted, err := sup.RestartTask(id)
	if err != nil {
		t.Fatalf("restart task: %v", err)
	}
	defer func() { _ = sup.StopTask(id) }()

	if got := restarted.Meta().CreatedAt; !got.Equal(created) {
		t.Errorf("created_at changed across restart: want %s, got %s", created, got)
	}
	if got := restarted.Meta().StartTime; !got.After(start) {
		t.Errorf("start_time not advanced across restart: want after %s, got %s", start, got)
	}
}

// TestSupervisor_RestartMissingCwdKeepsTask 覆盖「工作目录已被删除时重启：返回可读错误，
// 任务保持原状态，日志不受损」。
func TestSupervisor_RestartMissingCwdKeepsTask(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	cwd := t.TempDir()
	task, err := sup.StartTask("echo missing-cwd-probe", cwd, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	waitTaskStatus(t, task, StatusExited, 5*time.Second)
	id := task.Meta().ID
	logPath := task.Meta().LogPath

	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log before restart: %v", err)
	}

	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}

	if _, err := sup.RestartTask(id); err == nil {
		t.Fatalf("expected an error restarting with a missing working directory")
	} else if !strings.Contains(err.Error(), cwd) {
		t.Errorf("error should name the missing working directory, got: %v", err)
	}

	if got := task.Meta().Status; got != StatusExited {
		t.Errorf("task status changed after failed restart: want %s, got %s", StatusExited, got)
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log after failed restart: %v", err)
	}
	if !strings.Contains(string(before), "missing-cwd-probe") || !strings.Contains(string(after), "missing-cwd-probe") {
		t.Errorf("log content damaged by failed restart: before=%q after=%q", before, after)
	}
}

// TestSupervisor_RestartAppendsToExistingLog 覆盖「重启后 run.log 保留重启前的内容（追加而非截断）」。
func TestSupervisor_RestartAppendsToExistingLog(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("echo restart-append-line", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	waitTaskStatus(t, task, StatusExited, 5*time.Second)
	id := task.Meta().ID
	logPath := task.Meta().LogPath

	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log before restart: %v", err)
	}
	if got := strings.Count(string(before), "restart-append-line"); got != 1 {
		t.Fatalf("expected 1 log line before restart, got %d: %q", got, before)
	}

	restarted, err := sup.RestartTask(id)
	if err != nil {
		t.Fatalf("restart task: %v", err)
	}
	waitTaskStatus(t, restarted, StatusExited, 5*time.Second)

	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log after restart: %v", err)
	}
	if got := strings.Count(string(after), "restart-append-line"); got != 2 {
		t.Errorf("expected the pre-restart log content to be preserved and appended to, got %d lines: %q", got, after)
	}
}

// readTaskMeta 读取磁盘上的任务元数据,用于断言持久化的策略与时间字段。
func readTaskMeta(t *testing.T, baseDir, id string) TaskMeta {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(baseDir, id, "meta.json"))
	if err != nil {
		t.Fatalf("read meta %s: %v", id, err)
	}
	var m TaskMeta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal meta %s: %v", id, err)
	}
	return m
}

// TestSupervisor_ReplayAlwaysRestartsAlwaysAndSkipsNo 覆盖「执行方启动后,策略为 always 的
// 已停止任务自动回到运行态,策略为 no 的任务保持停止」。优雅停机用 StopAll 模拟:它只把状态
// 写成 stopped、绝不动策略,因此 stopped 也在重放范围内(KTD10)。
func TestSupervisor_ReplayAlwaysRestartsAlwaysAndSkipsNo(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	alwaysTask, err := sup.StartTaskWithPolicy("sleep 30", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start always task: %v", err)
	}
	noTask, err := sup.StartTaskWithPolicy("sleep 30", "", nil, RestartNo, nil)
	if err != nil {
		t.Fatalf("start no task: %v", err)
	}
	alwaysID, noID := alwaysTask.Meta().ID, noTask.Meta().ID

	// 优雅停机:状态写成 stopped,策略保留。
	sup.StopAll()
	if got := readTaskMeta(t, dir, alwaysID).Restart; got != RestartAlways {
		t.Fatalf("graceful shutdown must not touch policy, got %q", got)
	}
	if got := readTaskMeta(t, dir, alwaysID).Status; got != StatusStopped {
		t.Fatalf("graceful shutdown should leave stopped, got %s", got)
	}

	// 执行方下次启动:全新 supervisor 从磁盘加载并重放。
	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	defer sup2.StopAll()

	if results := sup2.ReplayAlways(); len(results) != 0 {
		t.Fatalf("unexpected replay failures: %+v", results)
	}

	replayed, err := sup2.GetTask(alwaysID)
	if err != nil {
		t.Fatalf("get replayed always task: %v", err)
	}
	if got := replayed.Meta().Status; got != StatusRunning {
		t.Errorf("always task not replayed: want running, got %s", got)
	}
	if !isProcessAlive(replayed.Meta().PID) {
		t.Errorf("replayed always task pid %d is not alive", replayed.Meta().PID)
	}
	if got := replayed.Meta().Restart; got != RestartAlways {
		t.Errorf("replay must preserve the policy, got %q", got)
	}

	untouched, err := sup2.GetTask(noID)
	if err != nil {
		t.Fatalf("get no task: %v", err)
	}
	if got := untouched.Meta().Status; got == StatusRunning {
		t.Errorf("no task should not be replayed, got %s", got)
	}
}

// TestSupervisor_ReplayAlwaysIgnoresExitCode 覆盖「策略为 always 的任务退出码为 0 时,执行方
// 下次启动仍重放它」(与 docker 一致,不做退出码门控)。
func TestSupervisor_ReplayAlwaysIgnoresExitCode(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTaskWithPolicy("echo replay-zero-exit", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	waitTaskStatus(t, task, StatusExited, 5*time.Second)
	id := task.Meta().ID
	if got := readTaskMeta(t, dir, id).ExitCode; got != 0 {
		t.Fatalf("probe command should exit 0, got %d", got)
	}

	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	if results := sup2.ReplayAlways(); len(results) != 0 {
		t.Fatalf("unexpected replay failures: %+v", results)
	}

	replayed, err := sup2.GetTask(id)
	if err != nil {
		t.Fatalf("get replayed task: %v", err)
	}
	waitTaskStatus(t, replayed, StatusExited, 5*time.Second)
	logData, err := os.ReadFile(replayed.Meta().LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := strings.Count(string(logData), "replay-zero-exit"); got != 2 {
		t.Errorf("exit-0 always task should be replayed once more, got %d log lines: %q", got, logData)
	}
}

// TestSupervisor_ReplayAlwaysSingleFailureLogsAndContinues 覆盖「某个 always 任务因工作目录
// 缺失无法重放:该任务保持停止、原因出现在它的日志里,其他 always 任务照常被拉起」(R8)。
func TestSupervisor_ReplayAlwaysSingleFailureLogsAndContinues(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	cwd := t.TempDir()
	bad, err := sup.StartTaskWithPolicy("echo replay-bad", cwd, nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start bad task: %v", err)
	}
	waitTaskStatus(t, bad, StatusExited, 5*time.Second)
	badID := bad.Meta().ID

	good, err := sup.StartTaskWithPolicy("sleep 30", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start good task: %v", err)
	}
	goodID := good.Meta().ID

	sup.StopAll()
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}

	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	defer sup2.StopAll()

	results := sup2.ReplayAlways()
	if len(results) != 1 || results[0].ID != badID {
		t.Fatalf("expected exactly the missing-cwd task to fail, got %+v", results)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), cwd) {
		t.Errorf("failure should name the missing cwd, got %v", results[0].Err)
	}

	failed, err := sup2.GetTask(badID)
	if err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	if got := failed.Meta().Status; got == StatusRunning {
		t.Errorf("failed replay must leave the task stopped, got %s", got)
	}
	logData, err := os.ReadFile(failed.Meta().LogPath)
	if err != nil {
		t.Fatalf("read failed task log: %v", err)
	}
	if !strings.Contains(string(logData), "[replay]") || !strings.Contains(string(logData), cwd) {
		t.Errorf("replay failure reason missing from task log: %q", logData)
	}

	okTask, err := sup2.GetTask(goodID)
	if err != nil {
		t.Fatalf("get good task: %v", err)
	}
	if got := okTask.Meta().Status; got != StatusRunning {
		t.Errorf("other always task should still be replayed, got %s", got)
	}
}

// TestSupervisor_StopTaskExplicitClearsRestartPolicy 覆盖「带 always 的任务被显式停止后,
// 元数据里的策略变为 no,且执行方下次启动不重放它」(R7/KD5)。
func TestSupervisor_StopTaskExplicitClearsRestartPolicy(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTaskWithPolicy("sleep 30", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID

	if err := sup.StopTaskAndClearPolicy(id); err != nil {
		t.Fatalf("stop with policy clear: %v", err)
	}
	if got := task.Meta().Restart; got != RestartNo {
		t.Errorf("in-memory policy not cleared: got %q", got)
	}
	if got := readTaskMeta(t, dir, id).Restart; got != RestartNo {
		t.Errorf("persisted policy not cleared: got %q", got)
	}
	if isProcessAlive(task.Meta().PID) {
		t.Errorf("process %d should be dead after explicit stop", task.Meta().PID)
	}

	// 执行方下次启动不重放它。
	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	if results := sup2.ReplayAlways(); len(results) != 0 {
		t.Fatalf("unexpected replay results: %+v", results)
	}
	again, err := sup2.GetTask(id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got := again.Meta().Status; got == StatusRunning {
		t.Errorf("explicitly stopped always task must not be replayed, got %s", got)
	}
}

// TestSupervisor_ReplayAlwaysRestartsRunningTask 覆盖 KTD10 的 running 分支:执行方崩溃后
// 遗留的存活进程(新进程读到 running 且 PID 存活)也会被重放 —— 先停掉遗留进程再重新拉起,
// 不会同时存在两个实例。
func TestSupervisor_ReplayAlwaysRestartsRunningTask(t *testing.T) {
	dir := t.TempDir()
	sup1, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 1: %v", err)
	}

	task, err := sup1.StartTaskWithPolicy("sleep 30", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID
	oldPID := task.Meta().PID

	// 模拟执行方崩溃后重新拉起:新进程读到的记录里 running 且 PID 存活。
	sup2, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor 2: %v", err)
	}
	defer sup2.StopAll()

	if results := sup2.ReplayAlways(); len(results) != 0 {
		t.Fatalf("unexpected replay failures: %+v", results)
	}

	replayed, err := sup2.GetTask(id)
	if err != nil {
		t.Fatalf("get replayed task: %v", err)
	}
	if isProcessAlive(oldPID) {
		t.Errorf("leftover process %d still alive after replay", oldPID)
	}
	newPID := replayed.Meta().PID
	if newPID == oldPID {
		t.Errorf("replay reused the leftover pid %d", oldPID)
	}
	if !isProcessAlive(newPID) {
		t.Errorf("replayed process %d is not alive", newPID)
	}
}

// TestSupervisor_ReplayAlwaysEmptyDirIsNoop 覆盖「执行方启动时没有任何任务目录:正常启动,无错误」。
func TestSupervisor_ReplayAlwaysEmptyDirIsNoop(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	if results := sup.ReplayAlways(); len(results) != 0 {
		t.Fatalf("empty dir replay should be a no-op, got %+v", results)
	}
	if got := len(sup.ListTasks(true)); got != 0 {
		t.Errorf("expected no tasks, got %d", got)
	}
}

// waitForFile 等一个由被测任务进程写出的标记文件出现。用作时序断言的事件同步点,
// 而不是"睡一会儿大概就到了"。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("marker file %s never appeared", path)
}

// TestSupervisor_RestartKeepsPolicyClearedDuringStopWait 覆盖 #15:RestartTask 先取元数据快照
// 再停旧实例,而停旧实例的等待最长可到 4.5s(1.5s 杀进程树 + 3s 有界等待)。若入站的 relay stop
// 落在这段窗口里,它改的是快照之后的策略,照抄快照安装新实例就等于把操作员刚被告知停掉的任务
// 复活,并让它在每次执行方启动时被重放。策略必须在安装点现读,让更晚的"停"赢。
//
// 时序用事件钉住、不靠 sleep 猜:任务脚本先装好 SIGTERM trap 再写 ready 标记(测试等它出现才起跑),
// 收到 SIGTERM 时写 term 标记。killProcessTree 发完 SIGTERM 还有 1.5s 宽限期才 SIGKILL,
// 所以 term 标记一出现就断言 RestartTask 正处于那段窗口内,此刻执行 stop。窗口余量由生产代码
// 的常量保证(1.5s),stop 侧只做几次内存操作。
func TestSupervisor_RestartKeepsPolicyClearedDuringStopWait(t *testing.T) {
	baseDir := t.TempDir()
	sup, err := New(baseDir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	defer sup.StopAll()

	workDir := t.TempDir()
	ready := filepath.Join(workDir, "ready")
	termMark := filepath.Join(workDir, "term")
	// trap 在先、ready 在后:ready 一出现,SIGTERM 必定会被记进 termMark。
	cmd := fmt.Sprintf(`trap 'printf term > "%s"' TERM; printf ready > "%s"; while :; do sleep 0.05; done`, termMark, ready)

	task, err := sup.StartTaskWithPolicy(cmd, workDir, nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}
	id := task.Meta().ID
	oldPID := task.Meta().PID

	waitForFile(t, ready, 5*time.Second)

	restarted := make(chan *Task, 1)
	restartErr := make(chan error, 1)
	go func() {
		nt, err := sup.RestartTask(id)
		if err != nil {
			restartErr <- err
			return
		}
		restarted <- nt
	}()

	waitForFile(t, termMark, 5*time.Second)
	// 此刻 RestartTask 卡在 killProcessTree 的宽限期里:模拟这时落地的入站 relay stop。
	if err := sup.StopTaskAndClearPolicy(id); err != nil {
		t.Fatalf("stop task while restart is waiting: %v", err)
	}

	select {
	case nt := <-restarted:
		if got := nt.Meta().Restart; got != RestartNo {
			t.Errorf("replacement restart policy = %q, want %q: a stop landing during the restart wait must win", got, RestartNo)
		}
		// 重启照常安装运行实例,只是带上最新策略。
		if got := nt.Meta().Status; got != StatusRunning {
			t.Errorf("replacement status = %q, want %q", got, StatusRunning)
		}
		if got := nt.Meta().PID; got == oldPID {
			t.Errorf("restart reused the old pid %d", oldPID)
		}
	case err := <-restartErr:
		t.Fatalf("restart task: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("restart did not finish")
	}

	// 落盘的记录同样不能带回 always,否则执行方每次启动都会把它重放回来。
	if got := readTaskMeta(t, baseDir, id).Restart; got != RestartNo {
		t.Errorf("persisted restart policy = %q, want %q", got, RestartNo)
	}
}
