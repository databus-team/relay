// Package jobrunner runs config-defined jobs on the local machine. It backs the
// `relay job run` command so a user can manually trigger the same exec /
// file_delete jobs the remote watcher runs, e.g. applying a pulled patch.
package jobrunner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/watcher"
)

// Result carries the captured stdout/stderr of an executed job.
type Result struct {
	Stdout string
	Stderr string
}

// Run executes the job with the given ID from watchCfg.Jobs locally. When file
// is non-empty it is bound to the {file_path}, {file_name}, {file_dir} and
// {file_remote_path} variables. Conditions (job.If) are deliberately ignored:
// a manual run has no preceding job results to evaluate against.
func Run(ctx context.Context, watchCfg *config.WatchConfig, jobID, file string) (Result, error) {
	var job *config.JobConfig
	for i := range watchCfg.Jobs {
		if watchCfg.Jobs[i].ID == jobID {
			job = &watchCfg.Jobs[i]
			break
		}
	}
	if job == nil {
		return Result{}, fmt.Errorf("job %q not found in workspace %q", jobID, watchCfg.ID)
	}

	vars := buildVars(file)

	switch job.Type {
	case "exec":
		if file == "" && usesFileVars(job.Cmd) {
			return Result{}, fmt.Errorf("job %q uses a file variable; provide a file argument", jobID)
		}
		cmd := watcher.SubstituteVariables(job.Cmd, vars)
		cwd := job.Cwd
		if cwd == "" {
			cwd = watchCfg.LocalDir
		}
		cwd = watcher.SubstituteVariables(cwd, vars)

		stdout, stderr, code := runLocalCommandCapture(cmd, cwd, job.Timeout)
		if code != 0 {
			return Result{Stdout: stdout, Stderr: stderr}, fmt.Errorf("exec job %q failed (exit %d): %s", jobID, code, strings.TrimSpace(stderr))
		}
		return Result{Stdout: stdout, Stderr: stderr}, nil

	case "file_delete":
		if file == "" {
			return Result{}, fmt.Errorf("job %q deletes a file; provide a file argument", jobID)
		}
		path := watcher.SubstituteVariables(job.Path, vars)
		if err := os.Remove(path); err != nil {
			return Result{}, fmt.Errorf("file_delete job %q failed: %w", jobID, err)
		}
		return Result{}, nil

	default:
		return Result{}, fmt.Errorf("unknown job type: %q", job.Type)
	}
}

// buildVars constructs the substitution variables for a manual run. file_path
// and friends bind to the local file argument (when given); otherwise only the
// timestamp is provided.
func buildVars(file string) map[string]string {
	vars := map[string]string{
		"timestamp": time.Now().Format(time.RFC3339),
	}
	if file == "" {
		return vars
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	vars["file_path"] = abs
	vars["file_remote_path"] = abs
	vars["file_name"] = filepath.Base(abs)
	vars["file_dir"] = filepath.Dir(abs)
	return vars
}

// usesFileVars reports whether s references any of the file-binding variables.
func usesFileVars(s string) bool {
	for _, token := range []string{"{file_path}", "{file_name}", "{file_dir}", "{file_remote_path}"} {
		if strings.Contains(s, token) {
			return true
		}
	}
	return false
}

// runLocalCommandCapture runs cmd in sh with an optional cwd and timeout
// (seconds), returning captured stdout, stderr, and the exit code.
func runLocalCommandCapture(cmd, cwd string, timeout int) (string, string, int) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	ec := exec.CommandContext(ctx, "sh", "-c", cmd)
	if cwd != "" {
		ec.Dir = cwd
	}

	var stdout, stderr bytes.Buffer
	ec.Stdout = &stdout
	ec.Stderr = &stderr

	err := ec.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return stdout.String(), stderr.String(), exitErr.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), stderr.String() + " (timed out)", 124
		}
		return stdout.String(), stderr.String() + " (" + err.Error() + ")", 1
	}
	return stdout.String(), stderr.String(), 0
}
