// Package jobrunner runs config-defined jobs on the local machine. It backs the
// `relay job run` command so a user can manually trigger the same exec /
// file_delete jobs the remote watcher runs, e.g. applying a pulled patch.
package jobrunner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/watcher"
)

// Result carries the captured stdout/stderr of an executed job, plus the raw
// exit code and wall duration so callers can render step progress/status.
type Result struct {
	JobID    string
	Type     string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	// Err 是执行层面的失败(non-zero/未知类型等);nil 表示成功。
	Err error
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
	return runOne(ctx, watchCfg, job, file)
}

// runOne executes a single resolved job and returns its Result (with exit code,
// duration and captured output) and a wrapped error on failure.
func runOne(ctx context.Context, watchCfg *config.WatchConfig, job *config.JobConfig, file string) (Result, error) {
	start := time.Now()
	done := func(r Result, err error) (Result, error) {
		r.Duration = time.Since(start)
		r.Err = err
		return r, err
	}

	vars := buildVars(file)

	switch job.Type {
	case "exec":
		if file == "" && (usesFileVars(job.Cmd) || usesFileVars(job.Cwd)) {
			return done(Result{}, fmt.Errorf("job %q uses a file variable; provide a file argument", job.ID))
		}
		cmd := watcher.SubstituteVariables(job.Cmd, vars)
		cwd := job.Cwd
		if cwd == "" {
			cwd = watchCfg.LocalDir
		}
		cwd = watcher.SubstituteVariables(cwd, vars)

		stdout, stderr, code := watcher.RunLocalCommandCapture(cmd, cwd, job.Timeout)
		res := Result{Stdout: stdout, Stderr: stderr, ExitCode: code}
		if code != 0 {
			return done(res, fmt.Errorf("exec job %q failed (exit %d): %s", job.ID, code, strings.TrimSpace(stderr)))
		}
		return done(res, nil)

	// 删除类工作不再有专有 job 类型:统一用 exec 执行删除命令(如 `rm -f {file_path}`),
	// 幂等且任一路径 fetch/push 语义一致。未知类型一律报错。

	default:
		return done(Result{}, fmt.Errorf("unknown job type: %q", job.Type))
	}
}

// Step is one job's progress/status report emitted by RunJobs. Running is true
// for the "about to start" report and false once the completed Result is set.
type Step struct {
	Total   int
	Running bool
	Result  Result
}

// RunJobs runs every job in watchCfg.Jobs sequentially, invoking report twice
// per job once "about to start" (Running=true) and once when it finishes with
// its Result set. Conditions (job.If) are deliberately ignored, mirroring Run.
// Returns the overall exit code: 0 if all steps succeeded, else the first
// failing job's exit code (or 1 when it carried none).
func RunJobs(ctx context.Context, watchCfg *config.WatchConfig, file string, report func(Step)) int {
	total := len(watchCfg.Jobs)
	for i := range watchCfg.Jobs {
		job := &watchCfg.Jobs[i]
		report(Step{Total: total, Running: true, Result: Result{JobID: job.ID, Type: job.Type}})
		res, err := runOne(ctx, watchCfg, job, file)
		res.JobID = job.ID
		res.Type = job.Type
		report(Step{Total: total, Result: res})
		if err != nil {
			if res.ExitCode == 0 {
				res.ExitCode = 1
			}
			return res.ExitCode
		}
	}
	return 0
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
	// 与 watcher.go 的 buildVars 保持一致:路径类变量转正斜杠(ToSlash)。
	// 否则 Windows 原生的反斜杠路径(如 D:\Group_Projects\…)填进 cmd 后,
	// 经 sh -c 执行时反斜杠被 sh 当作转义符吞掉 -> 命令里路径分隔符全丢,
	// git 等收到 D:Group_Projects… 打不开(`sh` 层 MTC 是 sh -c)。
	for _, name := range fileVarNames {
		switch name {
		case "file_path", "file_remote_path":
			vars[name] = filepath.ToSlash(abs)
		case "file_name":
			vars[name] = filepath.Base(abs)
		case "file_dir":
			vars[name] = filepath.ToSlash(filepath.Dir(abs))
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
