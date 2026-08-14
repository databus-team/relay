// Package jobrunner runs config-defined jobs on the local machine. It backs the
// `relay job run` command so a user can manually trigger the same exec /
// file_delete jobs the remote watcher runs, e.g. applying a pulled patch.
package jobrunner

import (
	"context"
	"fmt"
	"os"
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
		if file == "" && (usesFileVars(job.Cmd) || usesFileVars(job.Cwd)) {
			return Result{}, fmt.Errorf("job %q uses a file variable; provide a file argument", jobID)
		}
		cmd := watcher.SubstituteVariables(job.Cmd, vars)
		cwd := job.Cwd
		if cwd == "" {
			cwd = watchCfg.LocalDir
		}
		cwd = watcher.SubstituteVariables(cwd, vars)

		stdout, stderr, code := watcher.RunLocalCommandCapture(cmd, cwd, job.Timeout)
		if code != 0 {
			return Result{Stdout: stdout, Stderr: stderr}, fmt.Errorf("exec job %q failed (exit %d): %s", jobID, code, strings.TrimSpace(stderr))
		}
		return Result{Stdout: stdout, Stderr: stderr}, nil

	case "file_delete":
		if file == "" && usesFileVars(job.Path) {
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

// fileVarNames is the single source of truth for the file-binding variable
// names, shared by buildVars and usesFileVars so the two can't drift apart.
var fileVarNames = []string{"file_path", "file_name", "file_dir", "file_remote_path"}

// buildVars constructs the substitution variables for a manual run. file_path
// and friends bind to the local file argument (when provided); otherwise only
// the timestamp is provided.
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
	for _, name := range fileVarNames {
		switch name {
		case "file_path", "file_remote_path":
			vars[name] = abs
		case "file_name":
			vars[name] = filepath.Base(abs)
		case "file_dir":
			vars[name] = filepath.Dir(abs)
		}
	}
	return vars
}

// usesFileVars reports whether s references any of the file-binding variables.
func usesFileVars(s string) bool {
	for _, name := range fileVarNames {
		if strings.Contains(s, "{"+name+"}") {
			return true
		}
	}
	return false
}
