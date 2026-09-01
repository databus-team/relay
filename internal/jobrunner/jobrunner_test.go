package jobrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/user/relay/internal/config"
)

func baseWatch() *config.WatchConfig {
	return &config.WatchConfig{
		ID: "demo",
		// No LocalDir by default so exec jobs run in the process cwd; the
		// LocalDir-default behavior is covered separately in
		// TestRun_ExecDefaultsToLocalDir.
		Jobs: []config.JobConfig{
			{ID: "apply", Type: "exec", Cmd: "echo {file_path}"},
			{ID: "status", Type: "exec", Cmd: "echo hello"},
			{ID: "pwd", Type: "exec", Cmd: "echo $PWD"},
		},
	}
}

func TestRun_ExecBindsFilePath(t *testing.T) {
	file := "/abs/path/to/a.patch"
	res, err := Run(context.Background(), baseWatch(), "apply", file)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != file {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(res.Stdout), file)
	}
}

func TestRun_ExecNeedsFileButNoneGiven(t *testing.T) {
	_, err := Run(context.Background(), baseWatch(), "apply", "")
	if err == nil || !strings.Contains(err.Error(), "file variable") {
		t.Fatalf("expected a file-required error, got: %v", err)
	}
}

func TestRun_ExecWithoutFileVarsRuns(t *testing.T) {
	res, err := Run(context.Background(), baseWatch(), "status", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(res.Stdout), "hello")
	}
}

func TestRun_ExecFailureReturnsError(t *testing.T) {
	watch := baseWatch()
	watch.Jobs = []config.JobConfig{{ID: "boom", Type: "exec", Cmd: "exit 3"}}
	if _, err := Run(context.Background(), watch, "boom", ""); err == nil {
		t.Fatal("expected error for failing exec job")
	}
}

func TestRun_ExecDefaultsToLocalDir(t *testing.T) {
	watch := baseWatch()
	dir := t.TempDir()
	watch.LocalDir = dir

	res, err := Run(context.Background(), watch, "pwd", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != dir {
		t.Errorf("exec cwd = %q, want %q", got, dir)
	}
}

func TestRun_ExecNeedsFileWhenCwdReferencesFile(t *testing.T) {
	watch := baseWatch()
	watch.Jobs = []config.JobConfig{{ID: "go", Type: "exec", Cmd: "pwd", Cwd: "{file_dir}"}}
	if _, err := Run(context.Background(), watch, "go", ""); err == nil {
		t.Fatal("expected a file-required error for a job referencing a file var in cwd")
	}
}

func TestRun_UnknownJobType(t *testing.T) {
	watch := baseWatch()
	watch.Jobs = []config.JobConfig{{ID: "x", Type: "nope"}}
	if _, err := Run(context.Background(), watch, "x", ""); err == nil {
		t.Fatal("expected error for unknown job type")
	}
}

func TestRun_UnknownJobID(t *testing.T) {
	if _, err := Run(context.Background(), baseWatch(), "missing", ""); err == nil {
		t.Fatal("expected error for unknown job id")
	}
}

func TestRunJobs_EmitsStepProgressAndExitCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.patch")
	if err := os.WriteFile(path, []byte("patch"), 0o644); err != nil {
		t.Fatal(err)
	}

	watch := baseWatch()
	// apply(exec) -> clean(exec) -> then a failing exec step.
	watch.Jobs = []config.JobConfig{
		{ID: "apply", Type: "exec", Cmd: "echo {file_path}"},
		{ID: "clean", Type: "exec", Cmd: "echo cleaned"},
		{ID: "boom", Type: "exec", Cmd: "exit 3"},
	}

	var steps []Step
	exit := RunJobs(context.Background(), watch, path, func(s Step) {
		s.Result.Stdout = "" // 只关心标记/状态,不比较输出
		steps = append(steps, s)
	})
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}

	// 事件序列:每步一个 Running + 一个 finished(含最终失败步)。
	if len(steps) != 6 {
		t.Fatalf("got %d step events, want 6 (3 jobs x start/finish)", len(steps))
	}
	for idx, s := range steps {
		wantRunning := idx%2 == 0
		if s.Running != wantRunning {
			t.Errorf("event %d Running = %v, want %v", idx, s.Running, wantRunning)
		}
		if s.Total != 3 {
			t.Errorf("event %d Total = %d, want 3", idx, s.Total)
		}
	}
	// index5 = 第 3 步(boom)的 finished,应携带错误与 JobID。
	finish := steps[5]
	if finish.Result.Err == nil {
		t.Error("expected boom step to carry an error")
	}
	if finish.Result.JobID != "boom" {
		t.Errorf("failing step JobID = %q, want boom", finish.Result.JobID)
	}
}
