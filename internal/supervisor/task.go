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

	"github.com/user/relay/internal/portfwd"
)

type TaskStatus string

const (
	StatusRunning TaskStatus = "running"
	StatusExited  TaskStatus = "exited"
	StatusStopped TaskStatus = "stopped"
	StatusFailed  TaskStatus = "failed"
)

// TaskMeta 描述可序列化的任务元数据。
// Ports 以结构化规格落盘(含绑定主机,R21);升级前记录的 ports 是展示字符串数组,
// 由 portfwd.Spec 的反序列化按历史默认绑定 127.0.0.1 还原,旧记录无需迁移(KTD9)。
type TaskMeta struct {
	ID        string         `json:"id"`
	Cmd       string         `json:"cmd"`
	Cwd       string         `json:"cwd"`
	PID       int            `json:"pid"`
	Ports     []portfwd.Spec `json:"ports,omitempty"`
	Status    TaskStatus     `json:"status"`
	ExitCode  int            `json:"exit_code"`
	StartTime time.Time      `json:"start_time"`
	EndTime   *time.Time     `json:"end_time,omitempty"`
	LogPath   string         `json:"log_path"`
	// Restart 是任务的 restart 策略（always|no）；空值按 no 读。首次运行或重启都保留记录。
	Restart string `json:"restart,omitempty"`
	// CreatedAt 是首次创建时间，重启时保持不变；StartTime 表示最近一次启动。
	CreatedAt time.Time `json:"created_at"`
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
	// superseded 标记该任务对象已被同 id 的新实例取代:它的收尾不再写回记录。旧实例的收尾
	// goroutine 在新实例落盘之后才 persist 时,会把 status/pid/end_time 覆盖回旧值,而两个
	// Task 各持自己的锁,没有任何东西能给这两次写排序。
	superseded bool
}

func newTask(id, cmdStr, cwd string, dir string) *Task {
	return newTaskWithPorts(id, cmdStr, cwd, nil, dir)
}

func newTaskWithPorts(id, cmdStr, cwd string, ports []portfwd.Spec, dir string) *Task {
	now := time.Now()
	return &Task{
		meta: TaskMeta{
			ID:        id,
			Cmd:       cmdStr,
			Cwd:       cwd,
			Ports:     ports,
			Status:    StatusRunning,
			StartTime: now,
			CreatedAt: now,
			LogPath:   filepath.Join(dir, "run.log"),
		},
		dir:       dir,
		listeners: make(map[chan string]struct{}),
		done:      make(chan struct{}),
	}
}

// Meta 返回任务元数据的副本。
func (t *Task) Meta() TaskMeta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.meta
}

// markSuperseded 声明该任务对象已被同 id 的新实例取代,其生命周期收尾不再写回 meta.json。
// 仅由同 id 重启路径在拉起替代实例之前调用。
func (t *Task) markSuperseded() {
	t.mu.Lock()
	t.superseded = true
	t.mu.Unlock()
}

// setRestartPolicy 更新并持久化任务的 restart 策略。仅由显式的入站 relay stop 路径调用
// (StopTaskAndClearPolicy);执行方优雅停机走 StopAll,绝不触碰策略(R7/KD5)。
func (t *Task) setRestartPolicy(policy string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 记录里的旧值按同一规则读出来就是目标值时不必重写:空值与 no 语义相同,从没带过
	// --restart 的任务被 stop 时不再产生第二次磁盘写。
	if NormalizeRestart(t.meta.Restart) == NormalizeRestart(policy) {
		return
	}
	t.meta.Restart = policy
	_ = t.persist()
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
