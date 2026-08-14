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
			{ID: "clean", Type: "file_delete", Path: "{file_path}"},
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

func TestRun_FileDeleteHardcodedPathNeedsNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.log")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	watch := baseWatch()
	watch.Jobs = []config.JobConfig{{ID: "purge", Type: "file_delete", Path: path}}
	if _, err := Run(context.Background(), watch, "purge", ""); err != nil {
		t.Fatalf("file_delete with a hardcoded path should not demand a file arg: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected hardcoded-path file to be deleted, stat err = %v", statErr)
	}
}

func TestRun_ExecNeedsFileWhenCwdReferencesFile(t *testing.T) {
	watch := baseWatch()
	watch.Jobs = []config.JobConfig{{ID: "go", Type: "exec", Cmd: "pwd", Cwd: "{file_dir}"}}
	if _, err := Run(context.Background(), watch, "go", ""); err == nil {
		t.Fatal("expected a file-required error for a job referencing a file var in cwd")
	}
}

func TestRun_FileDeleteDeletesLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.patch")
	if err := os.WriteFile(path, []byte("patch"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), baseWatch(), "clean", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected file to be deleted, stat err = %v", statErr)
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
