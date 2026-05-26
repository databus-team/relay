package watcher

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"golang.org/x/sync/errgroup"
)

type Watcher struct {
	cfg       *config.Config
	processed map[string]bool
	jobResults map[string]bool
}

func New(cfg *config.Config) (*Watcher, error) {
	return &Watcher{
		cfg:       cfg,
		processed: make(map[string]bool),
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

func (w *Watcher) heartbeat(ctx context.Context) {
	// Get command directory from backend config
	commandDir := "/commands"
	if dir, ok := w.cfg.Backend.Config["command_dir"].(string); ok && dir != "" && dir != "/" {
		commandDir = dir
	}

	heartbeatPath := commandDir + "/.heartbeat"

	// Create backend for heartbeat
	b, err := backend.NewBackend(w.cfg.Backend.Type, w.cfg.Backend.Config)
	if err != nil {
		log.Printf("Heartbeat: failed to create backend: %v", err)
		return
	}

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

	for _, watchCfg := range w.cfg.Watch {
		g.Go(func() error {
			return w.processWatch(ctx, watchCfg)
		})
	}

	return g.Wait()
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
				output, err := b.Exec(ctx, cmd, cwd)
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