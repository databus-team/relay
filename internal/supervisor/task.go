package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type TaskStatus string

const (
	StatusRunning TaskStatus = "running"
	StatusExited  TaskStatus = "exited"
	StatusStopped TaskStatus = "stopped"
	StatusFailed  TaskStatus = "failed"
)

// TaskMeta 描述可序列化的任务元数据
type TaskMeta struct {
	ID        string     `json:"id"`
	Cmd       string     `json:"cmd"`
	Cwd       string     `json:"cwd"`
	PID       int        `json:"pid"`
	Ports     []string   `json:"ports,omitempty"`
	Status    TaskStatus `json:"status"`
	ExitCode  int        `json:"exit_code"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	LogPath   string     `json:"log_path"`
}

// Task 托管的后台运行任务
type Task struct {
	meta      TaskMeta
	mu        sync.RWMutex
	dir       string
	cmd       *exec.Cmd
	logFile   *os.File
	listeners map[chan string]struct{}
	done      chan struct{}
}

func newTask(id, cmdStr, cwd string, dir string) *Task {
	return newTaskWithPorts(id, cmdStr, cwd, nil, dir)
}

func newTaskWithPorts(id, cmdStr, cwd string, ports []string, dir string) *Task {
	return &Task{
		meta: TaskMeta{
			ID:        id,
			Cmd:       cmdStr,
			Cwd:       cwd,
			Ports:     ports,
			Status:    StatusRunning,
			StartTime: time.Now(),
			LogPath:   filepath.Join(dir, "run.log"),
		},
		dir:       dir,
		listeners: make(map[chan string]struct{}),
		done:      make(chan struct{}),
	}
}

func (t *Task) Meta() TaskMeta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.meta
}

func (t *Task) broadcast(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch := range t.listeners {
		select {
		case ch <- line:
		default:
			// 避免慢监听者阻塞主日志流
		}
	}
}

func (t *Task) persist() error {
	data, err := json.MarshalIndent(t.meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(t.dir, "meta.json")
	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, metaPath)
}

// Stop 干净终止正在运行的任务
func (t *Task) Stop() error {
	t.mu.Lock()
	if t.meta.Status != StatusRunning {
		t.mu.Unlock()
		return nil
	}
	pid := t.meta.PID
	t.meta.Status = StatusStopped
	now := time.Now()
	t.meta.EndTime = &now
	t.mu.Unlock()

	err := killProcessTree(pid)
	t.mu.Lock()
	_ = t.persist()
	t.mu.Unlock()
	return err
}

// StreamLogs 读取历史日志并支持可选的流式监听
func (t *Task) StreamLogs(ctx context.Context, follow bool, tailLines int, onChunk func(string)) error {
	t.mu.RLock()
	logPath := t.meta.LogPath
	status := t.meta.Status
	t.mu.RUnlock()

	// 1. 读取已有历史日志行
	if logPath != "" {
		lines, err := readLastLines(logPath, tailLines)
		if err == nil {
			for _, line := range lines {
				onChunk(line)
			}
		}
	}

	// 2. 如果不需要 follow 或进程已退出，直接返回
	if !follow || status != StatusRunning {
		return nil
	}

	// 3. 注册流式监听器
	ch := make(chan string, 128)
	t.mu.Lock()
	// 双重检查进程状态
	if t.meta.Status != StatusRunning {
		t.mu.Unlock()
		return nil
	}
	t.listeners[ch] = struct{}{}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.listeners, ch)
		t.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-ch:
			if !ok {
				return nil
			}
			onChunk(line)
		case <-t.done:
			// 进程结束，把 channel 中残余日志排空
			for {
				select {
				case line := <-ch:
					onChunk(line)
				default:
					return nil
				}
			}
		}
	}
}

func readLastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var all []string
	scanner := bufio.NewScanner(f)
	// 允许单行最大 1MB
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		all = append(all, scanner.Text()+"\n")
	}

	if n <= 0 || len(all) <= n {
		return all, scanner.Err()
	}
	return all[len(all)-n:], scanner.Err()
}

// pipeToLog 持续消费 reader 并写入文件与广播通道
func (t *Task) pipeToLog(r io.Reader) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		text := scanner.Text() + "\n"
		t.mu.Lock()
		if t.logFile != nil {
			_, _ = t.logFile.WriteString(text)
		}
		t.mu.Unlock()
		t.broadcast(text)
	}
}
