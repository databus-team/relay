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
	GetCommandDir() string
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