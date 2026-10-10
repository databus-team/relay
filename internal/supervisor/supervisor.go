package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

var (
	ErrTaskNotFound = errors.New("task not found")
)

type Supervisor struct {
	baseDir string
	mu      sync.RWMutex
	tasks   map[string]*Task
}

func DefaultBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".relay", "tasks")
	}
	return filepath.Join(home, ".relay", "tasks")
}

// New 创建并初始化 Supervisor
func New(baseDir string) (*Supervisor, error) {
	if baseDir == "" {
		baseDir = DefaultBaseDir()
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create supervisor dir: %w", err)
	}

	s := &Supervisor{
		baseDir: baseDir,
		tasks:   make(map[string]*Task),
	}

	s.loadExistingTasks()
	return s, nil
}

func (s *Supervisor) loadExistingTasks() {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskDir := filepath.Join(s.baseDir, e.Name())
		metaBytes, err := os.ReadFile(filepath.Join(taskDir, "meta.json"))
		if err != nil {
			continue
		}
		var meta TaskMeta
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			continue
		}

		// 存活状态校准：若记录为 running 但实际系统进程不存在，标记为 exited。
		// 若记录为 running 且系统进程存活，说明这是旧 agent 重启或异常退出遗留的无主孤儿进程（已断开 I/O 管道）。
		// 主动终止该孤儿进程树，避免无主进程霸占端口与系统资源。
		if meta.Status == StatusRunning {
			if isProcessAlive(meta.PID) {
				_ = killProcessTree(meta.PID)
			}
			meta.Status = StatusStopped
			now := time.Now()
			meta.EndTime = &now
		}

		t := &Task{
			meta:      meta,
			dir:       taskDir,
			listeners: make(map[chan string]struct{}),
			done:      make(chan struct{}),
		}
		if meta.Status != StatusRunning {
			close(t.done)
		}
		s.tasks[meta.ID] = t
		_ = t.persist()
	}
}

// StopAll 优雅终止所有正在运行的后台任务（并发执行，缩短停机等待时间）
func (s *Supervisor) StopAll() {
	s.mu.RLock()
	var running []*Task
	for _, t := range s.tasks {
		if t.Meta().Status == StatusRunning {
			running = append(running, t)
		}
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, t := range running {
		wg.Add(1)
		go func(task *Task) {
			defer wg.Done()
			_ = task.Stop()
		}(t)
	}
	wg.Wait()
}

func generateTaskID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("t-%s", hex.EncodeToString(b))
}

func resolveShell() (string, []string) {
	if runtime.GOOS != "windows" {
		return "/bin/sh", nil
	}
	// Windows 优先寻找 Git Bash / MSYS
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		`C:\msys64\usr\bin\bash.exe`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("bash.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("sh.exe"); err == nil {
		return p, nil
	}
	return "cmd.exe", []string{"/c"}
}

// StartTask 启动一个后台守护任务
func (s *Supervisor) StartTask(cmdStr, cwd string, env []string) (*Task, error) {
	return s.StartTaskWithPorts(cmdStr, cwd, nil, env)
}

// StartTaskWithPorts 启动一个携带端口映射信息的后台守护任务
func (s *Supervisor) StartTaskWithPorts(cmdStr, cwd string, ports []string, env []string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := generateTaskID()
	taskDir := filepath.Join(s.baseDir, id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("create task dir: %w", err)
	}

	task := newTaskWithPorts(id, cmdStr, cwd, ports, taskDir)

	logFile, err := os.OpenFile(task.meta.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open task log: %w", err)
	}
	task.logFile = logFile

	shell, shellPrefix := resolveShell()
	var args []string
	if len(shellPrefix) > 0 {
		args = append(args, shellPrefix...)
		args = append(args, cmdStr)
	} else {
		args = []string{"-c", cmdStr}
	}

	cmd := exec.Command(shell, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(), env...)
	prepareCmdAttr(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}

	task.cmd = cmd
	task.meta.PID = cmd.Process.Pid
	if err := task.persist(); err != nil {
		_ = killProcessTree(cmd.Process.Pid)
		logFile.Close()
		return nil, fmt.Errorf("persist task meta: %w", err)
	}

	s.tasks[id] = task

	// 异步读取管道
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		task.pipeToLog(stdoutPipe)
	}()
	go func() {
		defer wg.Done()
		task.pipeToLog(stderrPipe)
	}()

	// 异步等待进程生命周期收尾
	go func() {
		wg.Wait()
		waitErr := cmd.Wait()

		task.mu.Lock()
		now := time.Now()
		task.meta.EndTime = &now
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				task.meta.ExitCode = exitErr.ExitCode()
			} else {
				task.meta.ExitCode = -1
			}
			if task.meta.Status != StatusStopped {
				task.meta.Status = StatusFailed
			}
		} else {
			task.meta.ExitCode = 0
			if task.meta.Status != StatusStopped {
				task.meta.Status = StatusExited
			}
		}

		if task.logFile != nil {
			_ = task.logFile.Sync()
			_ = task.logFile.Close()
			task.logFile = nil
		}
		_ = task.persist()
		close(task.done)
		task.mu.Unlock()
	}()

	return task, nil
}

// StopTask 终止指定任务
func (s *Supervisor) StopTask(id string) error {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()

	if !ok {
		return ErrTaskNotFound
	}
	return task.Stop()
}

// GetTask 获取指定任务
func (s *Supervisor) GetTask(id string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return task, nil
}

// ListTasks 列出所有任务元数据
func (s *Supervisor) ListTasks(all bool) []TaskMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []TaskMeta
	for _, t := range s.tasks {
		meta := t.Meta()
		if !all && meta.Status != StatusRunning {
			continue
		}
		list = append(list, meta)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].StartTime.After(list[j].StartTime)
	})
	return list
}

// StreamLogs 流式获取日志
func (s *Supervisor) StreamLogs(ctx context.Context, id string, follow bool, tailLines int, onChunk func(string)) error {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()

	if !ok {
		return ErrTaskNotFound
	}
	return task.StreamLogs(ctx, follow, tailLines, onChunk)
}
