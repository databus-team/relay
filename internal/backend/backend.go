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
