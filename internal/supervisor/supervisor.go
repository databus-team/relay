package supervisor

import (
	"bytes"
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

	"github.com/user/relay/internal/portfwd"
)

var (
	ErrTaskNotFound = errors.New("task not found")
)

// 任务的 restart 策略(R5)。策略为 always 的任务只在执行方进程启动时被重放(R6)。
const (
	RestartAlways = "always"
	RestartNo     = "no"
)

// NormalizeRestart 把策略归一为 always|no:空值与未知值按 no 读(KTD3,旧记录无需迁移)。
func NormalizeRestart(policy string) string {
	if policy == RestartAlways {
		return RestartAlways
	}
	return RestartNo
}

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
		changed := false
		if meta.Status == StatusRunning {
			if !isProcessAlive(meta.PID) {
				meta.Status = StatusExited
				now := time.Now()
				meta.EndTime = &now
				changed = true
			}
		}

		// 旧的记录没有首次创建时间，回退到最近一次启动时间（KTD3，无需迁移脚本）。
		if meta.CreatedAt.IsZero() {
			meta.CreatedAt = meta.StartTime
			changed = true
		}

		// 升级前的记录里 ports 是展示字符串数组，加载时已按默认绑定还原为结构化形态(R21/KTD9)；
		// 回写一次，记录才带上绑定面，后续 start/restart 才能无损重登记。
		legacyPorts := hasLegacyPorts(metaBytes)
		if legacyPorts {
			changed = true
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
		// 只在归一化真的改了记录时才回写:加载本身不该产生磁盘写。
		if changed {
			_ = t.persist()
		}
		// 旧形态的字符串里根本没有绑定主机,还原只能落在历史默认的 127.0.0.1,而回写让这次
		// 猜测固化到磁盘:若该任务升级前发布在非本机地址上,重登记就只绑本机了,且再也找不回
		// 原绑定面。这不是可自愈的情况,必须留一行痕,免得操作员只能从"连接被拒"反推。
		// 只登记、不失败加载,也不改变加载结果。
		if legacyPorts {
			s.appendTaskLog(meta.ID, fmt.Sprintf("[legacy-ports] task %s: pre-upgrade record stored ports without the bind host; bind defaults to %s (the published bind face may have narrowed and cannot be recovered)\n", meta.ID, portfwd.DefaultBind))
		}
	}
}

// hasLegacyPorts 报告记录里的 ports 是否还是升级前的展示字符串形态([]string),那种形态
// 丢掉了绑定面，加载后需要回写为结构化规格。
func hasLegacyPorts(metaBytes []byte) bool {
	var probe struct {
		Ports []json.RawMessage `json:"ports"`
	}
	if err := json.Unmarshal(metaBytes, &probe); err != nil {
		return false
	}
	for _, p := range probe.Ports {
		trimmed := bytes.TrimSpace(p)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			return true
		}
	}
	return false
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

// StartTaskWithPorts 启动一个携带端口转发规格的后台守护任务
func (s *Supervisor) StartTaskWithPorts(cmdStr, cwd string, ports []portfwd.Spec, env []string) (*Task, error) {
	return s.StartTaskWithPolicy(cmdStr, cwd, ports, RestartNo, env)
}

// StartTaskWithPolicy 在 StartTaskWithPorts 的基础上记录 restart 策略(KTD3、R5)。
func (s *Supervisor) StartTaskWithPolicy(cmdStr, cwd string, ports []portfwd.Spec, restart string, env []string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := generateTaskID()
	taskDir := filepath.Join(s.baseDir, id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("create task dir: %w", err)
	}

	task := newTaskWithPorts(id, cmdStr, cwd, ports, taskDir)
	task.meta.Restart = NormalizeRestart(restart)
	if err := s.launch(task, env); err != nil {
		return nil, err
	}
	return task, nil
}

// launch 打开任务日志、解析 shell、启动进程并挂上日志管道与生命周期收尾。调用方必须持有 s.mu。
// env 只随首次创建传入；重启按 R10 不重放首次运行的环境。
func (s *Supervisor) launch(task *Task, env []string) error {
	logFile, err := os.OpenFile(task.meta.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open task log: %w", err)
	}
	task.logFile = logFile

	shell, shellPrefix := resolveShell()
	var args []string
	if len(shellPrefix) > 0 {
		args = append(args, shellPrefix...)
		args = append(args, task.meta.Cmd)
	} else {
		args = []string{"-c", task.meta.Cmd}
	}

	cmd := exec.Command(shell, args...)
	if task.meta.Cwd != "" {
		cmd.Dir = task.meta.Cwd
	}
	cmd.Env = append(os.Environ(), env...)
	prepareCmdAttr(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		logFile.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start process: %w", err)
	}

	task.cmd = cmd
	task.meta.PID = cmd.Process.Pid
	if err := task.persist(); err != nil {
		_ = killProcessTree(cmd.Process.Pid)
		logFile.Close()
		return fmt.Errorf("persist task meta: %w", err)
	}

	s.tasks[task.meta.ID] = task

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
		// 已被同 id 新实例取代:收尾照做(日志同步关闭、完成信号关闭),但不写回记录 ——
		// 否则这份旧实例的 status/pid/end_time 会盖掉新实例刚写下的运行态。
		if !task.superseded {
			_ = task.persist()
		}
		close(task.done)
		task.mu.Unlock()
	}()

	return nil
}

// restartStopTimeout 是重启前等待旧实例退出的上限。对启动时认领的遗留进程不能等完成信号
// （那个信号只在本进程启动的任务上才会关闭），因此只做有界存活探测（KTD2、KTD10）。
const restartStopTimeout = 3 * time.Second

// RestartTask 在同一个 id、同一个任务目录上以记录中的命令、工作目录与端口重新运行任务（R9–R11）。
// 停掉旧实例与有界等待都在 s.mu 之外进行 —— 那一步最长可拖到 4.5s（1.5s 杀进程树 + 3s 有界等待），
// 不该把 ListTasks/StartTask 一起堵住。
func (s *Supervisor) RestartTask(id string) (*Task, error) {
	s.mu.RLock()
	prev, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrTaskNotFound
	}

	meta := prev.Meta()

	if meta.Cwd != "" {
		info, err := os.Stat(meta.Cwd)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("cannot restart task %s: working directory no longer exists: %s", id, meta.Cwd)
		}
	}

	// 该对象即将被替代:它的收尾不得再写回记录(A1)。
	prev.markSuperseded()

	// 仍在运行的实例（含执行方崩溃后认领的遗留进程）先停掉并有界等待其退出（R11）。
	if isProcessAlive(meta.PID) {
		_ = prev.Stop()
		waitProcessGone(meta.PID, restartStopTimeout)
	}

	taskDir := filepath.Join(s.baseDir, id)
	task := newTaskWithPorts(id, meta.Cmd, meta.Cwd, meta.Ports, taskDir)
	task.meta.CreatedAt = meta.CreatedAt

	s.mu.Lock()
	defer s.mu.Unlock()
	// 上一个实例不能被并发重启悄悄替换掉:装新实例前确认时间线上没有别的赢家。
	cur, ok := s.tasks[id]
	if !ok || cur != prev {
		return nil, fmt.Errorf("task %s was replaced by a concurrent restart", id)
	}
	// restart 策略在安装点现读,不认上面那份快照:停旧实例的等待最长可到 4.5s(1.5s 杀进程树 +
	// 3s 有界等待),这期间落地的入站 relay stop 会在同一个 prev 上把策略清成 no
	// (StopTaskAndClearPolicy),而快照里还是旧的 always —— 照抄快照就把操作员刚被告知停掉的
	// 任务复活了,并且每次执行方启动都被重放。重启照常安装运行实例,只是必须带上最新策略。
	task.meta.Restart = cur.Meta().Restart
	if err := s.launch(task, nil); err != nil {
		return nil, err
	}
	return task, nil
}

// waitProcessGone 以有界轮询等待进程退出。超时返回 false 后调用方仍以新进程继续。
// 轮询间隔按 ×1.5 退避到 200ms 上限:一次 3s 等待只需十余次探测,而不是 Windows 上每 20ms
// 一次 tasklist 的百来次。
func waitProcessGone(pid int, timeout time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	interval := 20 * time.Millisecond
	for {
		if !isProcessAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(interval)
		interval = time.Duration(float64(interval) * 1.5)
		if interval > 200*time.Millisecond {
			interval = 200 * time.Millisecond
		}
	}
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

// StopTaskAndClearPolicy 显式停止任务并清除其 restart 策略(R7/KD5):进程组被杀、记录与日志保留,
// 但任务不会再被执行方启动时的重放带回。仅由入站的 relay stop 调用;执行方自身的优雅停机走
// StopAll,它只写 stopped、绝不动策略。
func (s *Supervisor) StopTaskAndClearPolicy(id string) error {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()

	if !ok {
		return ErrTaskNotFound
	}
	err := task.Stop()
	// 即便进程已不在(Stop 空转)、或杀进程报错,策略也必须清除:停掉的对象不再自行复活。
	task.setRestartPolicy(RestartNo)
	return err
}

// RestartResult 描述一次启动重放的结果。
type RestartResult struct {
	ID  string
	Err error
}

// ReplayAlways 按策略重放任务(KTD10):只看策略、不看进入时的状态 —— exited/failed/stopped
// 都在执行方启动时重放,只有被显式 relay stop 清掉策略的才不重放。单条失败仅把原因追加进
// 该任务自己的日志并让它保持停止,其余任务继续(R8);返回失败清单供调用方记录。
func (s *Supervisor) ReplayAlways() []RestartResult {
	s.mu.RLock()
	var ids []string
	for id, t := range s.tasks {
		if NormalizeRestart(t.Meta().Restart) == RestartAlways {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()

	sort.Strings(ids)

	var results []RestartResult
	for _, id := range ids {
		if _, err := s.RestartTask(id); err != nil {
			s.appendTaskLog(id, fmt.Sprintf("[replay] failed to restore task: %v\n", err))
			results = append(results, RestartResult{ID: id, Err: err})
		}
	}
	return results
}

// appendTaskLog 把一行原因追加到任务自己的 run.log。用于重放失败留痕(R8):任务保持停止,
// 但用户能从 `relay logs <task-id>` 看到为什么没起来。
func (s *Supervisor) appendTaskLog(id, line string) {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		return
	}

	path := task.Meta().LogPath
	if path == "" {
		path = filepath.Join(s.baseDir, id, "run.log")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
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
