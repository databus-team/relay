package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type FsMcpBackend struct {
	url        string
	commandDir string
	headers    map[string]string
	httpClient *http.Client
}

type FsMcpConfig struct {
	URL         string            `mapstructure:"url"`
	Transport   string            `mapstructure:"transport"`
	Headers     map[string]string `mapstructure:"headers"`
	CommandDir  string            `mapstructure:"command_dir"`
}

func NewFsMcpBackend(config map[string]interface{}) (FileTransferBackend, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("fs-mcp backend requires 'url' config")
	}

	commandDir := "/commands"
	if dir, ok := config["command_dir"].(string); ok {
		commandDir = dir
	}

	headers := make(map[string]string)
	if h, ok := config["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}

	return &FsMcpBackend{
		url:        url,
		commandDir: commandDir,
		headers:    headers,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (b *FsMcpBackend) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fs_list_directory",
			"arguments": map[string]string{
				"path": path,
			},
		},
	}

	resp, err := b.doRequest(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to list directory: %w", err)
	}

	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Result.Content) == 0 {
		return []FileInfo{}, nil
	}

	// Parse the JSON text content into FileInfo array
	var files []FileInfo
	if err := json.Unmarshal([]byte(result.Result.Content[0].Text), &files); err != nil {
		return nil, fmt.Errorf("failed to parse file list: %w", err)
	}

	return files, nil
}

func (b *FsMcpBackend) Read(ctx context.Context, path string) ([]byte, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fs_read_file",
			"arguments": map[string]string{
				"path": path,
			},
		},
	}

	resp, err := b.doRequest(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Result.Content) == 0 {
		return nil, ErrNotFound
	}

	return []byte(result.Result.Content[0].Text), nil
}

func (b *FsMcpBackend) Write(ctx context.Context, path string, content []byte) error {
	// Try fs_create_file first (commonly used by fs-mcp)
	err := b.writeFile(ctx, "fs_create_file", path, content)
	if err == nil {
		return nil
	}

	// Fallback to fs_write_file
	return b.writeFile(ctx, "fs_write_file", path, content)
}

func (b *FsMcpBackend) writeFile(ctx context.Context, method string, path string, content []byte) error {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": method,
			"arguments": map[string]string{
				"path":    path,
				"content": string(content),
			},
		},
	}

	resp, err := b.doRequest(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Check if response contains error
	var result struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err == nil && result.Error != nil {
		return fmt.Errorf("mcp error: %s (code: %d)", result.Error.Message, result.Error.Code)
	}

	return nil
}

func (b *FsMcpBackend) Delete(ctx context.Context, path string) error {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fs_delete_file",
			"arguments": map[string]string{
				"path": path,
			},
		},
	}

	_, err := b.doRequest(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

func (b *FsMcpBackend) SupportsExec() bool {
	return true
}

func (b *FsMcpBackend) GetCommandDir() string {
	return b.commandDir
}

func (b *FsMcpBackend) doRequest(ctx context.Context, reqBody map[string]interface{}) ([]byte, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", b.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (b *FsMcpBackend) Stat(ctx context.Context, path string) (interface{}, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fs_stat",
			"arguments": map[string]string{
				"path": path,
			},
		},
	}

	resp, err := b.doRequest(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Result.Content) == 0 {
		return nil, ErrNotFound
	}

	return result.Result.Content[0].Text, nil
}

// Ensure fs_mcp backend is registered
func init() {
	RegisterBackend("fs-mcp", NewFsMcpBackend)
}