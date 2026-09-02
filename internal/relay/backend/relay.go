package backend

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/logx"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/version"
)

type RelayBackend struct {
	client     *client.Client
	watchDir   string
	watchID    string
	commandDir string
	execDir    string // push 落地根目录(执行方);空则用进程当前目录
	configPath string // config-sync 落盘目标(执行方)
	eventCh    chan backend.FileInfo
	mu         sync.RWMutex

	// tunnelAllow 是执行方 network_allow 出网白名单(已解析)。空 = 默认拒绝一切隧道建连(fail-closed)。
	tunnelAllow []config.TunnelRule

	// 注册成功日志只打一次(重连会反复 registerExecutor,信息本身每次都要发,
	// 但成功提示按需降级为 debug,避免刷屏)。
	regMu     sync.Mutex
	regLogged bool
}

// 执行方共享处理器(进程级):多个 RelayBackend 实例会共享同一 pooled client,
// 实例级字段会在命令轮询线程重启 backend 时被覆盖,故 push 落地后的本地 jobs 回调放全局。
var (
	pushJobsH  backend.PushJobHandler
	pushJobsMu sync.RWMutex

	// executorRole 标记本进程是否扮演「执行方」角色。executor 字段只在
	// relay watch(执行方进程)里生效;本地 CLI(push/exec/transport/sync/ping)
	// 即便配置写了 executor: true,也不注册为执行方、不处理入站请求,从而
	// relay exec 仍透明转发到远端 watch,且一份配置可安全地两端共用。
	executorRole   bool
	executorRoleMu sync.RWMutex
)

// SetExecutorRole 由 relay watch(执行方)进程在启动时置位;其余命令不调用。
func SetExecutorRole(on bool) {
	executorRoleMu.Lock()
	defer executorRoleMu.Unlock()
	executorRole = on
}

// executorRoleOn 返回本进程是否为执行方角色。
func executorRoleOn() bool {
	executorRoleMu.RLock()
	defer executorRoleMu.RUnlock()
	return executorRole
}

type Config struct {
	URL          string            `mapstructure:"url" yaml:"url"`
	Token        string            `mapstructure:"token" yaml:"token"`
	WatchID      string            `mapstructure:"watch_id" yaml:"watch_id"`
	WatchDir     string            `mapstructure:"watch_dir" yaml:"watch_dir"`
	CommandDir   string            `mapstructure:"command_dir" yaml:"command_dir"`
	ExecutorDir  string            `mapstructure:"executor_dir" yaml:"executor_dir"`
	Executor     bool              `mapstructure:"executor" yaml:"executor"`
	ConfigPath   string            `mapstructure:"config_path" yaml:"config_path"`     // 执行方 config-sync 落盘目标(缺省 ~/.relay/config.yaml)
	NetworkAllow []string          `mapstructure:"network_allow" yaml:"network_allow"` // 隧道出网白名单(host/IP/CIDR+端port);空=默认拒绝(fail-closed)
	Headers      map[string]string `mapstructure:"headers" yaml:"headers"`             // WS 握手自定义头(中转前置鉴权)
}

func NewRelayBackend(params map[string]interface{}) (backend.FileTransferBackend, error) {
	var cfg Config

	if url, ok := params["url"].(string); ok {
		cfg.URL = url
	}
	if token, ok := params["token"].(string); ok {
		cfg.Token = token
	}
	if watchID, ok := params["watch_id"].(string); ok {
		cfg.WatchID = watchID
	}
	if watchDir, ok := params["watch_dir"].(string); ok {
		cfg.WatchDir = watchDir
	}
	if commandDir, ok := params["command_dir"].(string); ok {
		cfg.CommandDir = commandDir
	}
	if exec, ok := params["executor"].(bool); ok {
		cfg.Executor = exec
	}
	if dir, ok := params["executor_dir"].(string); ok {
		cfg.ExecutorDir = dir
	}
	if cp, ok := params["config_path"].(string); ok {
		cfg.ConfigPath = cp
	}
	if raw, ok := params["network_allow"].([]interface{}); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				cfg.NetworkAllow = append(cfg.NetworkAllow, s)
			}
		}
	}
	if raw, ok := params["headers"].(map[string]interface{}); ok {
		cfg.Headers = make(map[string]string, len(raw))
		for k, v := range raw {
			if s, ok := v.(string); ok {
				cfg.Headers[k] = s
			}
		}
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
	if cfg.ConfigPath == "" {
		// 执行方 config-sync 的默认落盘目标(与 relay watch 的 ~/.relay/config.yaml 一致)。
		if home, err := os.UserHomeDir(); err == nil {
			cfg.ConfigPath = filepath.Join(home, ".relay", "config.yaml")
		}
	}

	var opts []client.Option
	if len(cfg.Headers) > 0 {
		h := make(http.Header, len(cfg.Headers))
		for k, v := range cfg.Headers {
			h.Set(k, v)
		}
		opts = append(opts, client.WithHeaders(h))
	}

	c, err := client.GetOrConnect(context.Background(), cfg.URL, cfg.Token, cfg.WatchID, opts...)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	b := &RelayBackend{
		client:     c,
		watchDir:   cfg.WatchDir,
		watchID:    cfg.WatchID,
		commandDir: cfg.CommandDir,
		execDir:    cfg.ExecutorDir,
		configPath: cfg.ConfigPath,
		eventCh:    make(chan backend.FileInfo, 100),
	}

	// 出网白名单:解析失败即配置错误,初始化即拒绝(不静默放行)。
	if allow, err := config.ParseNetworkAllowlist(cfg.NetworkAllow); err != nil {
		return nil, fmt.Errorf("network_allow: %w", err)
	} else {
		b.tunnelAllow = allow
	}

	if cfg.Executor && executorRoleOn() {
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
	exit, err := b.ExecStream(ctx, "", cmd, cwd, timeout, func(c backend.ExecChunk) { out.WriteString(c.Data) })
	if err != nil {
		return out.String(), err
	}
	if exit != 0 {
		return out.String(), fmt.Errorf("exit code %d", exit)
	}
	return out.String(), nil
}

// ExecStream 流式执行:本地 CLI 作为请求方时,经中转把命令转发给远端 executor 并实时回调输出。
// targetWatch 若非空即目标 executor 的注册 watch_id(workspace 绑定到该 executor 时);空=单根回退。
func (b *RelayBackend) ExecStream(ctx context.Context, targetWatch, cmd string, cwd string, timeout int, onChunk func(backend.ExecChunk)) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 0, err
	}
	if targetWatch == "" {
		targetWatch = b.watchID
	}

	resp, err := b.client.ExecStream(ctx, targetWatch, cmd, cwd, timeout, func(chunk protocol.ExecChunk) {
		if onChunk != nil {
			onChunk(backend.ExecChunk{Stdout: chunk.Stdout, Data: chunk.Data})
		}
	})
	if err != nil {
		return 0, err
	}
	return resp.ExitCode, nil
}

// Transport 将文件内容纯下发写到达远端执行方的目标路径(Jobs=false,不触发 workspace job)。
// 用于部署二进制等场景;targetWatch 指向执行方注册的 watch(通常是根 watch)。
func (b *RelayBackend) Transport(ctx context.Context, targetWatch, dest string, content []byte) error {
	if err := b.ensureConnected(ctx); err != nil {
		return err
	}
	resp, err := b.client.Transport(ctx, targetWatch, dest, content)
	if err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("transport to %s failed (exit=%d): %s", dest, resp.ExitCode, resp.Stderr)
	}
	return nil
}

// PushJob 将文件直达发到远端执行器并触发其本地 jobs;输出流式回调。无在线执行器时由中转兜底落地。
func (b *RelayBackend) PushJob(ctx context.Context, targetWatch, relPath string, content []byte, on func(backend.ExecChunk)) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 0, err
	}
	if targetWatch == "" {
		targetWatch = b.watchID
	}
	resp, err := b.client.PushJob(ctx, targetWatch, relPath, content, func(ch protocol.ExecChunk) {
		if on != nil {
			on(backend.ExecChunk{Stdout: ch.Stdout, Data: ch.Data})
		}
	})
	if err != nil {
		return 0, err
	}
	return resp.ExitCode, nil
}

// PushNoJobs 把内容纯下发到执行器的 dest 绝对路径(Jobs=false 的 transport 通道),
// 不触发任何 workspace job。对应 `relay push --no-jobs --dest <abs>`。
// targetWatch 为空时回退到本 watch(根)执行器。
func (b *RelayBackend) PushNoJobs(ctx context.Context, targetWatch, dest string, content []byte) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 0, err
	}
	if targetWatch == "" {
		targetWatch = b.watchID
	}
	resp, err := b.client.Transport(ctx, targetWatch, dest, content)
	if err != nil {
		return 0, err
	}
	return resp.ExitCode, nil
}

// TunnelOpen 打开一条到远端 exec 出网的隧道(本地 SOCKS5 端点按需调用)。`watchID` 声明选用
// 哪台 executor 的出口(与 exec/push/status 的工作区寻址一致);命中 watch 无在线执行方时,
// TunnelOpen 在建连确认即失败并返回可读错误(无可用执行方)。
func (b *RelayBackend) TunnelOpen(ctx context.Context, watchID, target string, port uint16) (*client.TunnelStream, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return nil, err
	}
	return b.client.TunnelOpen(ctx, watchID, target, port)
}

// ConfigSync 请求方把新配置经中转流式直达执行方落盘(WS 流式通道,不写 command 文件)。
// ConfigSync 经中转把配置流式直达指定执行方落盘(targetWatch 为其注册 watch_id;空=单根回退)。
func (b *RelayBackend) ConfigSync(ctx context.Context, targetWatch string, payload []byte) (int, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return 1, err
	}
	resp, err := b.client.ConfigSync(ctx, targetWatch, payload)
	if err != nil {
		return 1, err
	}
	return resp.ExitCode, nil
}

// UpgradeServer 请求方把本地 relay 二进制流式交付给中转发起「服务器自升级」,以中转自检
// 通过后的成功 ACK 为结算点返回 nil。二进制已由调用方读入 content。
func (b *RelayBackend) UpgradeServer(ctx context.Context, content []byte) error {
	if err := b.ensureConnected(ctx); err != nil {
		return err
	}
	return b.client.UpgradeServer(ctx, content)
}

// enableExecutor 让本 backend 在远端 relay watch 上扮演 executor:
// 注册为 watcherID 的 executor,并执行从中转转发来的 exec/push/config-sync 请求。
func (b *RelayBackend) enableExecutor() {
	b.client.SetExecHandler(func(sess *client.ExecSession) { b.handleInboundExec(sess) })
	b.client.SetPushJobHandler(func(sess *client.PushJobSession) { b.handleInboundPushJob(sess) })
	b.client.SetConfigSyncHandler(func(sess *client.ConfigSyncSession) { b.handleInboundConfigSync(sess) })
	b.client.SetTunnelHandler(func(sess *client.TunnelSession) { b.handleInboundTunnelConnect(sess) })
	b.client.SetOnReconnect(b.registerExecutor)
	b.registerExecutor()
}

// handleInboundConfigSync 执行方收到流式 config-sync:解码、校验、原子落盘到自身
// config_path,随后回执给请求方。复用 config.ApplyConfigFile 与文件命令交换同一份逻辑。
func (b *RelayBackend) handleInboundConfigSync(sess *client.ConfigSyncSession) {
	log.Printf("[config-sync] received config for watch %s", sess.WatchID())
	payload, err := base64.StdEncoding.DecodeString(sess.Payload())
	if err != nil {
		_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: "config-sync: bad base64: " + err.Error()})
		return
	}
	if err := config.ApplyConfigFile(payload, b.configPath); err != nil {
		log.Printf("[config-sync] apply failed: %v", err)
		_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: err.Error()})
		return
	}
	log.Printf("[config-sync] applied %d bytes -> %s (restart relay watch to take effect)", len(payload), b.configPath)
	_ = sess.Done(protocol.ExecResponse{ExitCode: 0, Stdout: fmt.Sprintf("config applied to %s; restart relay watch to take effect", b.configPath)})
}

// tunnelDialTimeout 是 executor 向目标发真实 TCP 拨号的上限,防止黑洞目标导致建连确认无界等待。
const tunnelDialTimeout = 10 * time.Second

// handleInboundTunnelConnect 执行方收到中转转发来的隧道建连:校验 network_allow 白名单,
// 命中则向**已校验的 IP 字面量**发起真实 TCP(dial 用 IP 而非原始 hostname,防 DNS 重绑 TOCTOU),
// 连成回执 OK 并交回给 session 泵双向字节流;未命中/dial 失败一律 Reject、不建任何内网连接(默认拒绝)。
// 空/缺省 network_allow → CheckTunnelTarget 恒不命中,全部拒绝(fail-closed,AE6)。
func (b *RelayBackend) handleInboundTunnelConnect(sess *client.TunnelSession) {
	ip, ok := config.CheckTunnelTarget(b.tunnelAllow, sess.Target(), int(sess.Port()))
	if !ok {
		log.Printf("[tunnel] deny target %s:%d (watch=%s) not in network_allow", sess.Target(), sess.Port(), sess.WatchID())
		sess.Reject(fmt.Sprintf("target %s:%d not allowed by network_allow", sess.Target(), sess.Port()))
		return
	}

	addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(sess.Port())))
	// 用有界超时拨号:黑洞/不可达目标在限定时间内报错,避免请求方无界等待建连确认。
	conn, err := net.DialTimeout("tcp", addr, tunnelDialTimeout)
	if err != nil {
		log.Printf("[tunnel] dial %s failed: %v", addr, err)
		sess.Reject("dial " + addr + ": " + err.Error())
		return
	}
	log.Printf("[tunnel] conn %s:%d via %s (watch=%s)", sess.Target(), sess.Port(), addr, sess.WatchID())
	sess.Accept(conn)
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
	log.Printf("[push] receiving %s (watch=%s, jobs=%v)", sess.RelPath, sess.WatchID, sess.Jobs)
	src := sess.Temp.Name()

	// Jobs=false:纯传输下发,内容写到目标路径(可为绝对路径)即完成,不跑 workspace job。
	if !sess.Jobs {
		dest, err := b.writeTransportFile(sess.RelPath, src)
		os.Remove(src)
		if err != nil {
			_ = sess.Write(false, "transport: write failed: "+err.Error()+"\n")
			_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: err.Error()})
			return
		}
		sess.AbsPath = dest
		log.Printf("[transport] delivered -> %s", dest)
		_ = sess.Done(protocol.ExecResponse{ExitCode: 0})
		return
	}

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
	log.Printf("[push] done %s -> %s (exit=%d)", sess.RelPath, absPath, exit)
	_ = sess.Done(protocol.ExecResponse{ExitCode: exit})
}

// writeTransportFile 把临时内容搬到目标路径 dest。dest 为绝对路径时直接写;
// 相对路径则以执行方 execDir 为根。用于纯传输下发(部署二进制等),不触发 jobs。
func (b *RelayBackend) writeTransportFile(dest, srcPath string) (string, error) {
	// Windows 上把 MSYS 绝对路径("/d/foo")转成原生盘符路径,再判绝对。
	if runtime.GOOS == "windows" {
		if w := msysToWindowsPath(dest); w != dest {
			dest = w
		}
	}
	if !filepath.IsAbs(dest) {
		base := b.execDir
		if base == "" {
			base = "."
		}
		dest = filepath.Join(base, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	s, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer s.Close()
	d, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	_, cerr := io.Copy(d, s)
	if cerr != nil {
		d.Close()
		return "", cerr
	}
	if cerr := d.Close(); cerr != nil {
		return "", cerr
	}
	return dest, nil
}

// writePushedFile 把 push 临时内容(srcPath)拷贝到执行方根目录下 relPath 的安全路径。
func (b *RelayBackend) writePushedFile(relPath, srcPath string) (string, error) {
	base := b.execDir
	if base == "" {
		base = "."
	}
	// Windows 上把 MSYS 绝对路径("/d/foo")转成原生盘符路径,再做 Abs。
	// 否则 filepath.Abs("/d/Group_Projects") 会解析成当前盘(C:)下的 \d\...,
	// 把 push 内容写进错误的目录(此处必须与 writeTransportFile 的转换保持一致)。
	if runtime.GOOS == "windows" {
		if w := msysToWindowsPath(base); w != base {
			base = w
		}
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
	if err := b.client.RegisterExecutor(ctx, b.watchID, "add", version.String()); err != nil {
		log.Printf("[relay] register executor for %s: %v", b.watchID, err)
		return
	}

	// 成功提示仅启动时打印一次;重连导致的重复注册以 debug 级别输出。
	b.regMu.Lock()
	first := !b.regLogged
	b.regLogged = true
	b.regMu.Unlock()
	if first {
		log.Printf("[relay] registered as executor for watch %s", b.watchID)
	} else {
		logx.Debugf("[relay] re-registered as executor for watch %s", b.watchID)
	}
}

// handleInboundExec 执行中转转发来的命令,并把输出流式写回,最后回 exit code。
func (b *RelayBackend) handleInboundExec(sess *client.ExecSession) {
	log.Printf("[exec] receiving cmd=%q cwd=%q timeout=%ds (watch=%s)", sess.Cmd(), sess.Cwd(), sess.Timeout(), sess.WatchID())
	start := time.Now()
	exit := runStream(sess.Cmd(), sess.Cwd(), sess.Timeout(), func(stdout bool, data string) {
		_ = sess.Write(stdout, data)
	})
	dur := time.Since(start)
	log.Printf("[exec] done exit=%d duration=%s (watch=%s)", exit, dur.Round(time.Millisecond), sess.WatchID())
	_ = sess.Done(protocol.ExecResponse{
		ExitCode: exit,
		Duration: dur.Milliseconds(),
	})
}

// normalizeExecDir 把路径在转换成为原生存档形式供 chdir:
// 在 Windows 上把 MSYS 风格 "/d/foo" 转成 "D:\foo",其余平台原样返回。
func normalizeExecDir(dir string) string {
	if runtime.GOOS != "windows" {
		return dir
	}
	return msysToWindowsPath(dir)
}

// msysToWindowsPath 把 MSYS 风格 "/d/foo" 转成 Windows "D:\foo";非这种形式原样返回。
// 独立函数便于跨平台单测。
func msysToWindowsPath(dir string) string {
	if len(dir) < 2 || dir[0] != '/' {
		return dir
	}
	if !isAlphaByte(dir[1]) || (len(dir) > 2 && dir[2] != '/') {
		return dir
	}
	drive := strings.ToUpper(string(dir[1]))
	rest := dir[2:]
	if rest == "" {
		return drive + `:\` // "/d" -> "D:\"
	}
	return drive + ":" + strings.ReplaceAll(rest, "/", "\\")
}

func isAlphaByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// runStream 在本地 sh -c 执行命令,边跑边 emit 输出,返回 exit code(-1 表示超时/上下文取消)。
func runStream(cmdStr, cwd string, timeout int, emit func(stdout bool, data string)) int {
	if timeout <= 0 {
		timeout = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// shell 与 PATH 按平台解析(Windows 上加固 msys PATH,避免起子进程解析不到 sh/工具)。
	shell, env := relayExecShell()
	c := exec.CommandContext(ctx, shell, "-c", cmdStr)
	c.Env = env
	// Windows 后台 daemon 下 spawn sh 默认会带出 cmd 控制台窗口闪烁,这里压掉。
	backend.HideConsoleWindow(c)
	// cwd 可能来自远端/本地 client(config 里 MSYS 风格 /d/...)。执行方要用自己平台
	// 能 chdir 的原生路径:在 Windows 上转成 D:\...。
	c.Dir = normalizeExecDir(cwd)
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

// Status 查询整座部署(中转 + 所有执行方)的连通性快照:本地→中转 段1(本地 Ping 计时)、
// 中转→各执行方 段2(中转过探针测得)以及本地累计(段1+段2)。执行方离线/探针超时时其
// Seg2 标不可用、Total 留空,整条命令仍成功;全部执行方离线时空列表,仍输出「中转正常、
// 执行方为空」。status 不针对单一执行方,故不再有 watch 参数。
func (b *RelayBackend) Status(ctx context.Context) (protocol.StatusResponse, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return protocol.StatusResponse{}, err
	}

	// 段1:本地→中继 → 本地 Ping 计时(往返 RTT)。
	seg1Start := time.Now()
	if err := b.client.Ping(ctx); err != nil {
		return protocol.StatusResponse{}, fmt.Errorf("ping transit: %w", err)
	}
	seg1 := time.Since(seg1Start).Milliseconds()

	st, err := b.client.Status(ctx)
	if err != nil {
		return protocol.StatusResponse{}, err
	}
	if !st.OK {
		return protocol.StatusResponse{}, fmt.Errorf("transit status failed: %s", st.Error)
	}

	st.Seg1 = protocol.StatusSegment{LatencyMS: &seg1}
	for i := range st.Executors {
		eh := &st.Executors[i]
		if eh.Seg2.LatencyMS != nil {
			total := seg1 + *eh.Seg2.LatencyMS
			eh.Total = protocol.StatusSegment{LatencyMS: &total}
		} else {
			eh.Total = protocol.StatusSegment{Unavailable: eh.Seg2.Unavailable}
		}
	}
	return st, nil
}

// Version 向中转查询版本台账(中转 + 各执行方),供 relay version 跨机对比。
// 该方法是 relay 后端特有;其它后端(如 local)不支持,由调用方按类型断言判断。
func (b *RelayBackend) Version(ctx context.Context) (protocol.VersionResponse, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return protocol.VersionResponse{}, err
	}
	return b.client.Version(ctx)
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

// Ensure RelayBackend implements PushNoJobsSender at compile time.
var _ backend.PushNoJobsSender = (*RelayBackend)(nil)
