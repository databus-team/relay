package backend

import (
	"context"
	"errors"
)

var (
	ErrNotSupported = errors.New("operation not supported by this backend")
	ErrNotFound     = errors.New("file not found")
	ErrPermission   = errors.New("permission denied")
)

type FileInfo struct {
	Name    string
	Path    string // 相对 backend watch 根的路径(事件驱动路由 workspace 用)
	IsDir   bool
	Size    int64
	ModTime string
}

type FileTransferBackend interface {
	ListDir(ctx context.Context, path string) ([]FileInfo, error)
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, content []byte) error
	Delete(ctx context.Context, path string) error
	SupportsExec() bool
	Exec(ctx context.Context, cmd string, cwd string, timeout int) (string, error)
	// Ping checks if remote watcher is alive by checking heartbeat file in commandDir
	Ping(ctx context.Context, commandDir, watchID string) error
}

// EventBackend is an optional interface for backends that support
// event-driven file watching. The watcher checks for this interface
// and uses events instead of polling when available.
type EventBackend interface {
	// Events returns a channel of file change events from the remote server.
	Events() <-chan FileInfo
	// SubscribeEvents starts receiving events for the configured watch.
	SubscribeEvents(ctx context.Context) error
}

// ExecChunk 单条增量执行输出块。
type ExecChunk struct {
	Stdout bool // true=stdout,false=stderr
	Data   string
}

// ExecStreamBackend 可选接口:backend 支持流式输出远程执行。
type ExecStreamBackend interface {
	// ExecStream 流式执行,onChunk 在每次收到增量输出时回调(nil 可忽略);返回 exit code。
	ExecStream(ctx context.Context, cmd string, cwd string, timeout int, onChunk func(ExecChunk)) (int, error)
}

// PushJobHandler 执行方在文件落地本地后,为某工作区跑 jobs 的处理器。
// out 用于把 job 输出回传请求方;返回 0 表示全部成功,非 0 表示有 job 失败。
type PushJobHandler func(watchID string, absPath string, out func(ExecChunk)) int

// PushJobCapable 可选接口:执行方后端可注册「push 文件落地后本地跑 jobs」的回调。
// 由远端 relay watch 注入,复用其自身的 watch 配置与 job 执行逻辑。
type PushJobCapable interface {
	SetPushJobHandler(h PushJobHandler)
}

// PushJobSender 可选接口：后端把文件直达远端执行方并触发 jobs(relay backend 实现);
// 无在线执行方时由中转兜底落地到暂存目录。
type PushJobSender interface {
	PushJob(ctx context.Context, relPath string, content []byte, on func(ExecChunk)) (int, error)
}

// ConfigSyncCapable 可选接口：后端支持经其原生通道(relay 为 WS 流式)把配置直接同步
// 到执行方落盘。runSync 据此决定走流式还是退回通用文件命令交换。
type ConfigSyncCapable interface {
	ConfigSync(ctx context.Context, payload []byte) (int, error) // 返回执行方 apply 的 exit code
}

type BackendFactory func(config map[string]interface{}) (FileTransferBackend, error)

var backends = make(map[string]BackendFactory)

func RegisterBackend(name string, factory BackendFactory) {
	backends[name] = factory
}

func NewBackend(backendType string, config map[string]interface{}) (FileTransferBackend, error) {
	factory, ok := backends[backendType]
	if !ok {
		return nil, errors.New("unknown backend type: " + backendType)
	}
	return factory(config)
}

func init() {
	RegisterBackend("local", NewLocalBackend)
}
