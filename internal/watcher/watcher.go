package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/exchange"
	"golang.org/x/sync/errgroup"
)

type Watcher struct {
	cfg        *config.Config
	processed  map[string]bool
	jobResults map[string]bool
}

func New(cfg *config.Config) (*Watcher, error) {
	return &Watcher{
		cfg:        cfg,
		processed:  make(map[string]bool),
		jobResults: make(map[string]bool),
	}, nil
}

func (w *Watcher) createBackend(watchCfg config.WatchConfig) (backend.FileTransferBackend, error) {
	return backend.NewBackend(w.cfg.Backend.Type, w.cfg.Backend.Config)
}

func (w *Watcher) Run(ctx context.Context) error {
	log.Println("Starting watcher with interval:", w.cfg.Interval, "seconds")

	ticker := time.NewTicker(time.Duration(w.cfg.Interval) * time.Second)
	defer ticker.Stop()

	// Start heartbeat goroutine
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go w.heartbeat(heartbeatCtx)

	// Start command processor goroutine (check every 2 seconds)
	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()
	go w.processCommandsLoop(cmdCtx)

	if err := w.runOnce(ctx); err != nil {
		log.Println("Initial watch error:", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				log.Printf("Watch error: %v", err)
			}
		}
	}
}

func (w *Watcher) processCommandsLoop(ctx context.Context) {
	minInterval := 2 * time.Second
	maxInterval := 30 * time.Second
	interval := minInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			hasWork, err := w.processCommands(ctx)
			if err != nil {
				log.Printf("Process commands error: %v", err)
			}
			if hasWork {
				interval = minInterval
			} else {
				interval *= 2
				if interval > maxInterval {
					interval = maxInterval
				}
			}
			stopped := timer.Stop()
			if !stopped {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
		}
	}
}

func (w *Watcher) heartbeat(ctx context.Context) {
	// Get command directory from backend config, default to /tmp/relay-commands
	commandDir := "/tmp/relay-commands"
	if dir, ok := w.cfg.Backend.Config["command_dir"].(string); ok && dir != "" && dir != "/" {
		commandDir = dir
	}

	// Create backend for heartbeat
	b, err := backend.NewBackend(w.cfg.Backend.Type, w.cfg.Backend.Config)
	if err != nil {
		log.Printf("Heartbeat: failed to create backend: %v", err)
		return
	}

	heartbeatPath := commandDir + "/.heartbeat"

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timestamp := time.Now().Format(time.RFC3339)
			if err := b.Write(context.Background(), heartbeatPath, []byte(timestamp)); err != nil {
				log.Printf("Heartbeat: failed to write heartbeat: %v", err)
			}
		}
	}
}

func (w *Watcher) RunOnce(ctx context.Context) error {
	return w.runOnce(ctx)
}

func (w *Watcher) RunOnceForWatch(ctx context.Context, watchID string) error {
	for _, watchCfg := range w.cfg.Watch {
		if watchCfg.ID == watchID {
			return w.processWatch(ctx, watchCfg)
		}
	}
	return fmt.Errorf("watch not found: %s", watchID)
}

func (w *Watcher) runOnce(ctx context.Context) error {
	g := &errgroup.Group{}

	// Process watch directories (user files)
	for i := range w.cfg.Watch {
		watchCfg := w.cfg.Watch[i]
		g.Go(func() error {
			return w.processWatch(ctx, watchCfg)
		})
	}

	return g.Wait()
}

func (w *Watcher) processCommands(ctx context.Context) (bool, error) {
	commandDir := "/tmp/relay-commands"
	if dir, ok := w.cfg.Backend.Config["command_dir"].(string); ok && dir != "" && dir != "/" {
		commandDir = dir
	}

	b, err := backend.NewBackend(w.cfg.Backend.Type, w.cfg.Backend.Config)
	if err != nil {
		return false, fmt.Errorf("failed to create backend for commands: %w", err)
	}

	files, err := b.ListDir(ctx, commandDir)
	if err != nil {
		// Command directory might not exist yet
		return false, nil
	}

	log.Printf("[commands] Found %d files in %s", len(files), commandDir)
	foundCmd := false
	for _, file := range files {
		if file.IsDir {
			continue
		}

		// Only process cmd-*.json files
		if !strings.HasPrefix(file.Name, "cmd-") || !strings.HasSuffix(file.Name, ".json") {
			continue
		}
		foundCmd = true

		log.Printf("[commands] Processing %s", file.Name)

		cmdPath := commandDir + "/" + file.Name
		if w.processed[cmdPath] {
			continue
		}

		w.processed[cmdPath] = true

		// Execute command
		result, err := w.executeCommand(ctx, cmdPath, b)
		if err != nil {
			log.Printf("Command failed %s: %v", file.Name, err)
			delete(w.processed, cmdPath)
		}

		// Write result file
		if result != nil {
			resultPath := commandDir + "/result-" + result.ID + ".json"
			result.CompletedAt = time.Now().Format(time.RFC3339)
			resultData, _ := json.Marshal(result)
			_ = b.Write(ctx, resultPath, resultData)
		}

		// Delete cmd file
		_ = b.Delete(ctx, cmdPath)
	}

	return foundCmd, nil
}

func (w *Watcher) executeCommand(ctx context.Context, cmdPath string, b backend.FileTransferBackend) (*exchange.ResultFile, error) {
	_ = ctx // keep ctx param for future use
	data, err := b.Read(ctx, cmdPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cmd file: %w", err)
	}

	var cmd exchange.CmdFile
	if err := json.Unmarshal(data, &cmd); err != nil {
		return nil, fmt.Errorf("failed to parse cmd file: %w", err)
	}

	if cmd.Cwd != "" {
		cmd.Cwd = config.NormalizeWindowsPath(cmd.Cwd)
	}

	log.Printf("[commands] Responder executing locally: %s (cwd: %s, timeout: %d)", cmd.Cmd, cmd.Cwd, cmd.Timeout)

	var result exchange.ResultFile
	result.ID = cmd.ID

	// Execute locally (this watcher acts as the responder)
	stdout, stderr, exitCode := runLocalCommandCapture(cmd.Cmd, cmd.Cwd, cmd.Timeout)
	result.Stdout = stdout
	result.Stderr = stderr
	result.ExitCode = exitCode

	if exitCode != 0 {
		log.Printf("[commands] Command failed (exit %d): %s", exitCode, strings.TrimSpace(stderr))
	} else {
		log.Printf("[commands] Command succeeded: %s", strings.TrimSpace(stdout))
	}

	return &result, nil
}

// runLocalCommandCapture runs cmd in shell with optional cwd and timeout (seconds).
// Returns stdout, stderr and exit code.
func runLocalCommandCapture(cmdStr, cwd string, timeout int) (string, string, int) {
	if cmdStr == "" {
		return "", "no command provided", 1
	}

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
	}

	execCmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	if cwd != "" {
		execCmd.Dir = cwd
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	execCmd.Stdout = &stdoutBuf
	execCmd.Stderr = &stderrBuf

	err := execCmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return stdoutBuf.String(), stderrBuf.String(), exitErr.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return stdoutBuf.String(), stderrBuf.String() + " (timed out)", 124
		}
		return stdoutBuf.String(), stderrBuf.String() + " (" + err.Error() + ")", 1
	}
	return stdoutBuf.String(), stderrBuf.String(), 0
}

func (w *Watcher) processWatch(ctx context.Context, watchCfg config.WatchConfig) error {
	b, err := w.createBackend(watchCfg)
	if err != nil {
		return fmt.Errorf("failed to create backend: %w", err)
	}

	files, err := b.ListDir(ctx, watchCfg.WatchDir)
	if err != nil {
		return fmt.Errorf("failed to list directory %s: %w", watchCfg.WatchDir, err)
	}

	for _, file := range files {
		if file.IsDir {
			continue
		}

		// Check if file matches any of the paths patterns
		if !w.matchAnyPattern(file.Name, watchCfg.Paths) {
			continue
		}

		filePath := watchCfg.WatchDir + "/" + file.Name
		if w.processed[filePath] {
			continue
		}

		w.processed[filePath] = true
		w.jobResults = make(map[string]bool)

		if err := w.executeJobs(ctx, watchCfg.Jobs, filePath, file.Name, watchCfg.WatchDir, watchCfg.LocalDir, b); err != nil {
			log.Printf("Job failed for %s: %v (file left untouched)", filePath, err)
			delete(w.processed, filePath)
		}
	}

	return nil
}

func (w *Watcher) matchAnyPattern(filename string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchPattern(filename, pattern) {
			return true
		}
	}
	return false
}

func (w *Watcher) executeJobs(ctx context.Context, jobs []config.JobConfig, filePath, fileName, watchDir, localDir string, b backend.FileTransferBackend) error {
	vars := w.buildVariables(filePath, fileName, watchDir)

	for _, job := range jobs {
		if job.If != "" {
			conditionMet, err := w.evaluateCondition(job.If)
			if err != nil {
				return fmt.Errorf("condition evaluation failed for %s: %w", job.If, err)
			}
			if !conditionMet {
				log.Printf("Skipping job %s (condition not met: %s)", job.ID, job.If)
				continue
			}
		}

		switch job.Type {
		case "exec":
			cmd := substituteVariables(job.Cmd, vars)
			cwd := job.Cwd
			if cwd == "" && localDir != "" {
				cwd = localDir
			}
			if cwd != "" {
				cwd = substituteVariables(cwd, vars)
			}

			// Try backend exec first (file exchange protocol)
			if b.SupportsExec() {
				// Use job timeout if provided
				timeout := job.Timeout
				output, err := b.Exec(ctx, cmd, cwd, timeout)
				if err != nil {
					w.jobResults[job.ID] = false
					return fmt.Errorf("exec job %s failed: %w", job.ID, err)
				}
				w.jobResults[job.ID] = true
				log.Printf("Exec job %s completed successfully: %s", job.ID, output)
			} else {
				// Fallback to local command execution
				if err := w.runLocalCommand(cmd, cwd); err != nil {
					w.jobResults[job.ID] = false
					return fmt.Errorf("exec job %s failed: %w", job.ID, err)
				}
				w.jobResults[job.ID] = true
				log.Printf("Exec job %s completed successfully (local)", job.ID)
			}

		case "file_delete":
			delPath := substituteVariables(job.Path, vars)
			if err := b.Delete(ctx, delPath); err != nil {
				w.jobResults[job.ID] = false
				return fmt.Errorf("file_delete job %s failed: %w", job.ID, err)
			}
			w.jobResults[job.ID] = true
			log.Printf("File delete job %s completed: %s", job.ID, delPath)

		default:
			return fmt.Errorf("unknown job type: %s", job.Type)
		}
	}

	return nil
}

func (w *Watcher) runLocalCommand(cmd, cwd string) error {
	execCmd := exec.Command("sh", "-c", cmd)
	if cwd != "" {
		execCmd.Dir = cwd
	}
	execCmd.Stdout = log.Writer()
	execCmd.Stderr = log.Writer()

	return execCmd.Run()
}

func (w *Watcher) evaluateCondition(cond string) (bool, error) {
	// Format: jobs.<job_id>.<state> where state is "success" or "failure"
	parts := strings.Split(cond, ".")
	if len(parts) < 3 {
		return false, fmt.Errorf("invalid condition format: %s", cond)
	}

	if parts[0] != "jobs" {
		return false, fmt.Errorf("invalid condition prefix: %s", parts[0])
	}

	jobID := strings.Join(parts[1:len(parts)-1], ".")
	state := parts[len(parts)-1]

	result, ok := w.jobResults[jobID]
	if !ok {
		return false, fmt.Errorf("job not found: %s", jobID)
	}

	if state == "success" {
		return result, nil
	}
	if state == "failure" {
		return !result, nil
	}

	return false, fmt.Errorf("invalid state: %s (expected success or failure)", state)
}

func (w *Watcher) buildVariables(filePath, fileName, watchDir string) map[string]string {
	return map[string]string{
		"file_path":        filePath,
		"file_name":        fileName,
		"file_dir":         filepath.Dir(filePath),
		"file_remote_path": filePath,
		"timestamp":        time.Now().Format(time.RFC3339),
	}
}

func matchPattern(filename, pattern string) bool {
	matched, _ := path.Match(pattern, filename)
	return matched
}

func substituteVariables(s string, vars map[string]string) string {
	result := s
	for key, val := range vars {
		result = strings.ReplaceAll(result, "{"+key+"}", val)
	}
	return result
}
