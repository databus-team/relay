package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/user/relay/internal/portfwd"
	"github.com/user/relay/internal/streamutil"
)

// logPollInterval 是跟随日志的兜底轮询间隔:通知可丢,丢了也不会超过这个时延。
const logPollInterval = 200 * time.Millisecond

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
	listeners map[chan struct{}]struct{}
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
		listeners: make(map[chan struct{}]struct{}),
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

// notify 叫醒跟随者。只送提示不带数据,日志内容由跟随者按偏移自己从 run.log 读。
func (t *Task) notify() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch := range t.listeners {
		select {
		case ch <- struct{}{}:
		default:
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

// StreamLogs 读取历史日志并可选跟随。数据来源是 run.log 本身,按偏移读取。
func (t *Task) StreamLogs(ctx context.Context, follow bool, tailLines int, onChunk func(string)) error {
	t.mu.RLock()
	logPath := t.meta.LogPath
	status := t.meta.Status
	dir := t.dir
	t.mu.RUnlock()

	if logPath == "" {
		logPath = filepath.Join(dir, "run.log")
	}
	if onChunk == nil {
		onChunk = func(string) {}
	}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}

	var pending []byte
	emit := func(data []byte) {
		if len(data) == 0 {
			return
		}
		pending = append(pending, data...)
		head, tail := streamutil.SplitIncompleteRune(pending)
		if len(head) > 0 {
			onChunk(string(head))
		}
		if len(tail) == 0 {
			pending = pending[:0]
		} else {
			pending = append(pending[:0], tail...)
		}
	}
	flush := func() {
		if len(pending) > 0 {
			onChunk(string(pending))
			pending = pending[:0]
		}
	}

	buf := make([]byte, streamutil.ChunkSize)
	offset := tailStartOffset(f, size, tailLines)
	for offset < size {
		n, err := f.ReadAt(buf, offset)
		if n > 0 {
			offset += int64(n)
			emit(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		if n == 0 {
			break
		}
	}

	if !follow || status != StatusRunning {
		flush()
		return nil
	}

	// 先注册通知再读文件,避免漏掉注册前写入的提示。
	notifyCh := make(chan struct{}, 1)
	t.mu.Lock()
	if t.meta.Status != StatusRunning {
		t.mu.Unlock()
		flush()
		return nil
	}
	t.listeners[notifyCh] = struct{}{}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.listeners, notifyCh)
		t.mu.Unlock()
	}()

	ticker := time.NewTicker(logPollInterval)
	defer ticker.Stop()

	drain := func() {
		for {
			n, err := f.ReadAt(buf, offset)
			if n > 0 {
				offset += int64(n)
				emit(buf[:n])
			}
			if err != nil || n == 0 {
				return
			}
		}
	}

	for {
		drain()
		select {
		case <-ctx.Done():
			flush()
			return ctx.Err()
		case <-t.done:
			drain()
			flush()
			return nil
		case <-notifyCh:
		case <-ticker.C:
		}
	}
}

// tailStartOffset 返回最后 tailLines 行的起始偏移;tailLines<=0 表示从头读。
// 从文件末尾按块回溯数换行,不把整份日志读进内存;回溯超过上限时宁可多给不少给。
func tailStartOffset(f *os.File, size int64, tailLines int) int64 {
	const blockSize = 32 * 1024
	const maxTailScanBytes = 32 * 1024 * 1024

	if tailLines <= 0 || size <= 0 {
		return 0
	}

	pos := size
	var buf []byte
	newlines := 0
	for pos > 0 && newlines <= tailLines {
		readSize := int64(blockSize)
		if readSize > pos {
			readSize = pos
		}
		start := pos - readSize
		chunk := make([]byte, readSize)
		n, err := f.ReadAt(chunk, start)
		if n > 0 {
			buf = append(chunk[:n], buf...)
			newlines += bytes.Count(chunk[:n], []byte{'\n'})
		}
		if err != nil && err != io.EOF {
			return 0
		}
		pos = start
		if len(buf) >= maxTailScanBytes {
			return pos
		}
	}

	// 末尾的换行不算一行内容。
	i := len(buf) - 1
	if i >= 0 && buf[i] == '\n' {
		i--
	}
	for left := tailLines; i >= 0; i-- {
		if buf[i] != '\n' {
			continue
		}
		left--
		if left == 0 {
			break
		}
	}
	return pos + int64(i+1)
}

// pipeToLog 把 reader 里的原始字节按到达顺序写进日志文件,并叫醒跟随者。
func (t *Task) pipeToLog(r io.Reader) {
	buf := make([]byte, streamutil.ChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.mu.Lock()
			if t.logFile != nil {
				_, _ = t.logFile.Write(buf[:n])
			}
			t.mu.Unlock()
			t.notify()
		}
		if err != nil {
			return
		}
	}
}
