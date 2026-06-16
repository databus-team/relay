package backend

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/client"
)

type RelayBackend struct {
	client     *client.Client
	watchDir   string
	watchID    string
	commandDir string
	eventCh    chan backend.FileInfo
	mu         sync.RWMutex
}

type Config struct {
	URL        string `mapstructure:"url" yaml:"url"`
	Token      string `mapstructure:"token" yaml:"token"`
	WatchID    string `mapstructure:"watch_id" yaml:"watch_id"`
	WatchDir   string `mapstructure:"watch_dir" yaml:"watch_dir"`
	CommandDir string `mapstructure:"command_dir" yaml:"command_dir"`
}

func NewRelayBackend(config map[string]interface{}) (backend.FileTransferBackend, error) {
	var cfg Config

	if url, ok := config["url"].(string); ok { cfg.URL = url }
	if token, ok := config["token"].(string); ok { cfg.Token = token }
	if watchID, ok := config["watch_id"].(string); ok { cfg.WatchID = watchID }
	if watchDir, ok := config["watch_dir"].(string); ok { cfg.WatchDir = watchDir }
	if commandDir, ok := config["command_dir"].(string); ok { cfg.CommandDir = commandDir }

	if cfg.WatchDir == "" { cfg.WatchDir = "." }
	if cfg.CommandDir == "" { cfg.CommandDir = "/tmp/relay-commands" }
	if cfg.WatchID == "" { cfg.WatchID = "default" }

	c, err := client.GetOrConnect(context.Background(), cfg.URL, cfg.Token, cfg.WatchID)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	return &RelayBackend{
		client:     c,
		watchDir:   cfg.WatchDir,
		watchID:    cfg.WatchID,
		commandDir: cfg.CommandDir,
		eventCh:    make(chan backend.FileInfo, 100),
	}, nil
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

func (b *RelayBackend) Exec(ctx context.Context, cmd string, cwd string, timeout int) (string, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return "", err
	}

	resp, err := b.client.Exec(ctx, cmd, cwd, timeout)
	if err != nil {
		return "", err
	}

	if resp.ExitCode != 0 {
		return resp.Stdout, fmt.Errorf("exit code %d: %s", resp.ExitCode, resp.Stderr)
	}

	return resp.Stdout, nil
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
