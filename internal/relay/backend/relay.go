package backend

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
)

type RelayBackend struct {
	client     *client.Client
	watchDir   string
	watchID    string
	commandDir string
	execDir    string // push 落地根目录(执行方);空则用进程当前目录
	eventCh    chan backend.FileInfo
	mu         sync.RWMutex
}

// 执行方共享处理器(进程级):多个 RelayBackend 实例会共享同一 pooled client,
// 实例级字段会在命令轮询线程重启 backend 时被覆盖,故 push 落地后的本地 jobs 回调放全局。
var (
	pushJobsH  backend.PushJobHandler
	pushJobsMu sync.RWMutex
)

type Config struct {
	URL         string `mapstructure:"url" yaml:"url"`
	Token       string `mapstructure:"token" yaml:"token"`
	WatchID     string `mapstructure:"watch_id" yaml:"watch_id"`
	WatchDir    string `mapstructure:"watch_dir" yaml:"watch_dir"`
	CommandDir  string `mapstructure:"command_dir" yaml:"command_dir"`
	ExecutorDir string `mapstructure:"executor_dir" yaml:"executor_dir"`
	Executor    bool   `mapstructure:"executor" yaml:"executor"`
}

func NewRelayBackend(config map[string]interface{}) (backend.FileTransferBackend, error) {
	var cfg Config

	if url, ok := config["url"].(string); ok {
		cfg.URL = url
	}
	if token, ok := config["token"].(string); ok {
		cfg.Token = token
	}
	if watchID, ok := config["watch_id"].(string); ok {
		cfg.WatchID = watchID
	}
	if watchDir, ok := config["watch_dir"].(string); ok {
		cfg.WatchDir = watchDir
	}
	if commandDir, ok := config["command_dir"].(string); ok {
		cfg.CommandDir = commandDir
	}
	if exec, ok := config["executor"].(bool); ok {
		cfg.Executor = exec
	}
	if dir, ok := config["executor_dir"].(string); ok {
		cfg.ExecutorDir = dir
	}

	if cfg.WatchDir == "" {
		cfg.WatchDir = "."
	}
	if cfg.CommandDir == "" {
		cfg.CommandDir = "/tmp/relay-commands"
	}
	if cfg.WatchID == "" {
		cfg.WatchID = "default"
	}

	c, err := client.GetOrConnect(context.Background(), cfg.URL, cfg.Token, cfg.WatchID)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	b := &RelayBackend{
		client:     c,
		watchDir:   cfg.WatchDir,
		watchID:    cfg.WatchID,
		commandDir: cfg.CommandDir,
		execDir:    cfg.ExecutorDir,
		eventCh:    make(chan backend.FileInfo, 100),
	}

	if cfg.Executor {
		b.enableExecutor()
	}

	return b, nil
}

func (b *RelayBackend) ensureConnected(ctx context.Context) error {
	if b.client.IsConnected() {
		return nil
	}
	return b.client.Connect(ctx)
}

func (b *RelayBackend) ListDir(ctx context.Context, path string) ([]backend.FileInfo, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return nil, err
	}

	entries, err := b.client.List(ctx, path)
	if err != nil {
		return nil, err
	}

	result := make([]backend.FileInfo, len(entries))
	for i, e := range entries {
		result[i] = backend.FileInfo{
			Name:    e.Name,
			IsDir:   e.IsDir,
			Size:    e.Size,
			ModTime: formatModTime(e.ModTime),
		}
	}
	return result, nil
}

func (b *RelayBackend) Read(ctx context.Context, path string) ([]byte, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return nil, err
	}
	return b.client.Pull(ctx, path)
}

func (b *RelayBackend) Write(ctx context.Context, path string, content []byte) error {
	if err := b.ensureConnected(ctx); err != nil {
		return err
	}
	return b.client.Push(ctx, path, content)
}

func (b *RelayBackend) Delete(ctx context.Context, path string) error {
	if err := b.ensureConnected(ctx); err != nil {
		return err
	}
	return b.client.Delete(ctx, path)
}

func (b *RelayBackend) SupportsExec() bool { return true }

// Exec 缓冲式执行并返回 stdout。
func (b *RelayBackend) Exec(ctx context.Context, cmd string, cwd string, timeout int) (string, error) {
	var out strings.Builder
	exit, err := b.ExecStream(ctx, cmd, cwd, timeout, func(c backend.ExecChunk) { out.WriteString(c.Data) })
	if err != nil {
		return out.String(), err
	}
	if exit != 0 {
		return out.String(), fmt.Errorf("exit code %d", exit)
	}
	return out.String(), nil
}

// ExecStream 流式执行:本地 CLI 作为请求方时,经中转把命令转发给远端 executor 并实时回调输出。
func (b *RelayBackend) ExecStream(ctx context.Context, cmd string, cwd string, timeout int, onChunk func(backend.ExecChunk)) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 0, err
	}

	resp, err := b.client.ExecStream(ctx, cmd, cwd, timeout, func(chunk protocol.ExecChunk) {
		if onChunk != nil {
			onChunk(backend.ExecChunk{Stdout: chunk.Stdout, Data: chunk.Data})
		}
	})
	if err != nil {
		return 0, err
	}
	return resp.ExitCode, nil
}

// PushJob 将文件直达远端执行方并触发其本地 jobs;输出流式回调。无在线执行方时由中转兜底落地。
func (b *RelayBackend) PushJob(ctx context.Context, relPath string, content []byte, on func(backend.ExecChunk)) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 0, err
	}
	resp, err := b.client.PushJob(ctx, relPath, content, func(ch protocol.ExecChunk) {
		if on != nil {
			on(backend.ExecChunk{Stdout: ch.Stdout, Data: ch.Data})
		}
	})
	if err != nil {
		return 0, err
	}
	return resp.ExitCode, nil
}

// enableExecutor 让本 backend(通常在远端 relay watch 上)扮演执行方:
// 注册为 watcherID 的 executor,并处理从中转转发来的 exec 请求。
func (b *RelayBackend) enableExecutor() {
	b.client.SetExecHandler(func(sess *client.ExecSession) { b.handleInboundExec(sess) })
	b.client.SetPushJobHandler(func(sess *client.PushJobSession) { b.handleInboundPushJob(sess) })
	b.client.SetOnReconnect(b.registerExecutor)
	b.registerExecutor()
}

// SetPushJobHandler 注册「push 文件落地后本地跑 jobs」的回调(由远端 relay watch 注入)。
func (b *RelayBackend) SetPushJobHandler(h backend.PushJobHandler) {
	pushJobsMu.Lock()
	pushJobsH = h
	pushJobsMu.Unlock()
}

func currentPushJobsHandler() backend.PushJobHandler {
	pushJobsMu.RLock()
	defer pushJobsMu.RUnlock()
	return pushJobsH
}

// handleInboundPushJob 收到转发来的流式 push-job:把临时落盘内容搬进本地 executor 目录,
// 再调用已注册的回调在远端跑该工作区的 jobs,并逐一 job 输出回流向请求方。
func (b *RelayBackend) handleInboundPushJob(sess *client.PushJobSession) {
	src := sess.Temp.Name()
	absPath, err := b.writePushedFile(sess.RelPath, src)
	os.Remove(src) // 临时文件已在会话结束前消费完,清理

	if err != nil {
		_ = sess.Write(false, "push job: write failed: "+err.Error()+"\n")
		_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: err.Error()})
		return
	}
	sess.AbsPath = absPath

	h := currentPushJobsHandler()

	if h == nil {
		_ = sess.Write(true, "[pushed to "+absPath+"; no jobs handler]\n")
		_ = sess.Done(protocol.ExecResponse{ExitCode: 0})
		return
	}

	out := func(ch backend.ExecChunk) { _ = sess.Write(ch.Stdout, ch.Data) }
	exit := h(sess.WatchID, sess.AbsPath, out)
	_ = sess.Done(protocol.ExecResponse{ExitCode: exit})
}

// writePushedFile 把 push 临时内容(srcPath)拷贝到执行方根目录下 relPath 的安全路径。
func (b *RelayBackend) writePushedFile(relPath, srcPath string) (string, error) {
	base := b.execDir
	if base == "" {
		base = "."
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(absBase, relPath))
	if !strings.HasPrefix(full, absBase+string(filepath.Separator)) && full != absBase {
		return "", fmt.Errorf("path traversal")
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "", err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return full, nil
}

// registerExecutor 向中转注册/重注册本客户端为其 watch 的执行方。
func (b *RelayBackend) registerExecutor() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.client.RegisterExecutor(ctx, b.watchID, "add"); err != nil {
		log.Printf("[relay] register executor for %s: %v", b.watchID, err)
	}
}

// handleInboundExec 执行中转转发来的命令,并把输出流式写回,最后回 exit code。
func (b *RelayBackend) handleInboundExec(sess *client.ExecSession) {
	start := time.Now()
	exit := runStream(sess.Cmd(), sess.Cwd(), sess.Timeout(), func(stdout bool, data string) {
		_ = sess.Write(stdout, data)
	})
	_ = sess.Done(protocol.ExecResponse{
		ExitCode: exit,
		Duration: time.Since(start).Milliseconds(),
	})
}

// runStream 在本地 sh -c 执行命令,边跑边 emit 输出,返回 exit code(-1 表示超时/上下文取消)。
func runStream(cmdStr, cwd string, timeout int, emit func(stdout bool, data string)) int {
	if timeout <= 0 {
		timeout = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	c.Dir = cwd
	c.Stdin = nil

	stdoutPipe, err := c.StdoutPipe()
	if err != nil {
		emit(false, "failed to open stdout: "+err.Error())
		return 1
	}
	stderrPipe, err := c.StderrPipe()
	if err != nil {
		emit(false, "failed to open stderr: "+err.Error())
		return 1
	}

	if err := c.Start(); err != nil {
		emit(false, err.Error())
		return 1
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			emit(true, sc.Text()+"\n")
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			emit(false, sc.Text()+"\n")
		}
	}()

	wg.Wait()
	c.Wait()

	if ctx.Err() != nil {
		return -1
	}
	return c.ProcessState.ExitCode()
}

func (b *RelayBackend) Ping(ctx context.Context, commandDir, watchID string) error {
	if !b.client.IsConnected() {
		return fmt.Errorf("not connected")
	}
	return b.client.Ping(ctx)
}

func (b *RelayBackend) Events() <-chan backend.FileInfo {
	return b.eventCh
}

func (b *RelayBackend) SubscribeEvents(ctx context.Context) error {
	if err := b.ensureConnected(ctx); err != nil {
		return err
	}

	if err := b.client.Subscribe(ctx, b.watchID); err != nil {
		return err
	}

	go b.forwardEvents()
	return nil
}

func (b *RelayBackend) forwardEvents() {
	for event := range b.client.EventCh() {
		fi := backend.FileInfo{
			Name:    event.Name,
			Path:    event.Path,
			IsDir:   false,
			Size:    event.Size,
			ModTime: formatModTime(event.ModTime),
		}
		select {
		case b.eventCh <- fi:
		default:
		}
	}
}

func (b *RelayBackend) Close() error {
	return b.client.Disconnect()
}

func formatModTime(unixMs int64) string {
	if unixMs == 0 {
		return ""
	}
	t := time.UnixMilli(unixMs)
	return t.Format(time.RFC3339)
}

func init() {
	backend.RegisterBackend("relay", NewRelayBackend)
}

// Ensure RelayBackend implements EventBackend at compile time.
var _ backend.EventBackend = (*RelayBackend)(nil)

// Ensure RelayBackend implements ExecStreamBackend at compile time.
var _ backend.ExecStreamBackend = (*RelayBackend)(nil)

// Ensure RelayBackend implements PushJobCapable at compile time.
var _ backend.PushJobCapable = (*RelayBackend)(nil)

// Ensure RelayBackend implements PushJobSender at compile time.
var _ backend.PushJobSender = (*RelayBackend)(nil)
