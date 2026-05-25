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

	"github.com/user/file-exchange/internal/backend"
	"github.com/user/file-exchange/internal/config"
	"golang.org/x/sync/errgroup"
)

type Watcher struct {
	cfg       *config.Config
	backend   backend.FileTransferBackend
	processed map[string]bool
	jobResults map[string]bool
}

func New(cfg *config.Config) (*Watcher, error) {
	backendCfg := cfg.Backend.Config
	if backendCfg == nil {
		backendCfg = make(map[string]interface{})
	}

	b, err := backend.NewBackend(cfg.Backend.Type, backendCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create backend: %w", err)
	}

	return &Watcher{
		cfg:       cfg,
		backend:   b,
		processed: make(map[string]bool),
		jobResults: make(map[string]bool),
	}, nil
}

func (w *Watcher) Run(ctx context.Context) error {
	log.Println("Starting watcher with interval:", w.cfg.Interval, "seconds")

	ticker := time.NewTicker(time.Duration(w.cfg.Interval) * time.Second)
	defer ticker.Stop()

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

func (w *Watcher) RunOnce(ctx context.Context) error {
	return w.runOnce(ctx)
}

func (w *Watcher) runOnce(ctx context.Context) error {
	g := &errgroup.Group{}

	for _, project := range w.cfg.Projects {
		g.Go(func() error {
			return w.processProject(ctx, project)
		})
	}

	return g.Wait()
}

func (w *Watcher) processProject(ctx context.Context, project config.ProjectConfig) error {
	watchDir := project.WatchDir
	pattern := project.FileMatch

	files, err := w.backend.ListDir(ctx, watchDir)
	if err != nil {
		return fmt.Errorf("failed to list directory %s: %w", watchDir, err)
	}

	log.Println("Found", len(files), "files in", watchDir)

	for _, file := range files {
		if file.IsDir {
			continue
		}

		matched := matchPattern(file.Name, pattern)
		if !matched {
			continue
		}

		filePath := watchDir + "/" + file.Name
		if w.processed[filePath] {
			continue
		}

		w.processed[filePath] = true
		w.jobResults = make(map[string]bool)

		if err := w.executeJobs(ctx, project.Jobs, filePath, file.Name, watchDir); err != nil {
			log.Printf("Job failed for %s: %v (file left untouched)", filePath, err)
			delete(w.processed, filePath)
			continue
		}

		for _, job := range project.Jobs {
			if job.Type == "file_delete" && !job.KeepFile {
				if err := w.backend.Delete(ctx, filePath); err != nil {
					log.Printf("Failed to delete file %s: %v", filePath, err)
				} else {
					log.Printf("Deleted file: %s", filePath)
				}
			}
		}
	}

	return nil
}

func (w *Watcher) executeJobs(ctx context.Context, jobs []config.JobConfig, filePath, fileName, watchDir string) error {
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

		vars := w.buildVariables(filePath, fileName, watchDir)

		switch job.Type {
		case "exec":
			cmd := substituteVariables(job.Cmd, vars)
			cwd := job.Cwd
			if cwd != "" {
				cwd = substituteVariables(cwd, vars)
			}

			if err := w.runLocalCommand(cmd, cwd); err != nil {
				w.jobResults[job.ID] = false
				return fmt.Errorf("exec job %s failed: %w", job.ID, err)
			}

			w.jobResults[job.ID] = true
			log.Printf("Exec job %s completed successfully", job.ID)

		case "file_delete":
			path := substituteVariables(job.Path, vars)
			if err := w.backend.Delete(ctx, path); err != nil {
				w.jobResults[job.ID] = false
				return fmt.Errorf("file_delete job %s failed: %w", job.ID, err)
			}
			w.jobResults[job.ID] = true
			log.Printf("File delete job %s completed: %s", job.ID, path)

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
	parts := strings.Split(cond, ".")
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid condition format: %s", cond)
	}

	if parts[0] != "jobs" {
		return false, fmt.Errorf("invalid condition prefix: %s", parts[0])
	}

	jobID := parts[1]
	result, ok := w.jobResults[jobID]
	if !ok {
		return false, fmt.Errorf("job not found: %s", jobID)
	}

	return result, nil
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
	if pattern == "" {
		return true
	}

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