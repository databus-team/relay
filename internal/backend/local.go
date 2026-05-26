package backend

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type LocalBackend struct {
	baseDir     string
	commandDir  string
}

func NewLocalBackend(config map[string]interface{}) (FileTransferBackend, error) {
	baseDir := "/"
	if dir, ok := config["base_dir"].(string); ok {
		baseDir = dir
	}

	commandDir := "/commands"
	if dir, ok := config["command_dir"].(string); ok {
		commandDir = dir
	}

	return &LocalBackend{
		baseDir:    baseDir,
		commandDir: commandDir,
	}, nil
}

func (b *LocalBackend) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	fullPath := filepath.Join(b.baseDir, path)

	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	result := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		result = append(result, FileInfo{
			Name:    entry.Name(),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}

	return result, nil
}

func (b *LocalBackend) Read(ctx context.Context, path string) ([]byte, error) {
	fullPath := filepath.Join(b.baseDir, path)

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return data, nil
}

func (b *LocalBackend) Write(ctx context.Context, path string, content []byte) error {
	fullPath := filepath.Join(b.baseDir, path)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(fullPath, content, 0644)
}

func (b *LocalBackend) ensureDir(path string) error {
	fullPath := filepath.Join(b.baseDir, path)
	return os.MkdirAll(fullPath, 0755)
}

func (b *LocalBackend) Delete(ctx context.Context, path string) error {
	fullPath := filepath.Join(b.baseDir, path)

	err := os.Remove(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}

	return nil
}

func (b *LocalBackend) SupportsExec() bool {
	return true
}

func (b *LocalBackend) Exec(ctx context.Context, cmd string, cwd string) (string, error) {
	execCmd := exec.Command("sh", "-c", cmd)
	if cwd != "" {
		execCmd.Dir = cwd
	}
	output, err := execCmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command failed: %w", err)
	}
	return string(output), nil
}

func (b *LocalBackend) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	fullPath := filepath.Join(b.baseDir, path)
	return os.Stat(fullPath)
}

func (b *LocalBackend) Ping(ctx context.Context, commandDir string) error {
	// Local backend always has watcher if this process is running
	return nil
}