package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type FsMcpBackend struct {
	url        string
	remoteRoot string
	commandDir string
	headers    map[string]string
	session    *mcp.ClientSession
}

// Global debug flag - can be set via CLI
var debugMode bool

// SetDebug enables or disables debug mode for fs-mcp backend
func SetDebug(enabled bool) {
	debugMode = enabled
}

func NewFsMcpBackend(config map[string]interface{}) (FileTransferBackend, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("fs-mcp backend requires 'url' config")
	}

	remoteRoot := "/"
	if root, ok := config["remote_root"].(string); ok {
		remoteRoot = root
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
		remoteRoot: remoteRoot,
		commandDir: commandDir,
		headers:    headers,
	}, nil
}

func (b *FsMcpBackend) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(b.remoteRoot, path)
}

// headerTransport wraps http.RoundTripper to add custom headers
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil {
		log.Printf("[fs-mcp debug] Request: %s %s, Status: %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return resp, err
}

func (b *FsMcpBackend) debugLog(format string, args ...any) {
	if debugMode {
		log.Printf("[fs-mcp] "+format, args...)
	}
}

func (b *FsMcpBackend) ensureConnection(ctx context.Context) error {
	if b.session != nil {
		return nil
	}

	// Create HTTP client with custom headers
	httpClient := &http.Client{
		Transport: &headerTransport{
			headers: b.headers,
			base:    http.DefaultTransport,
		},
	}

	// Create Streamable HTTP transport
	transport := &mcp.StreamableClientTransport{
		Endpoint:            b.url,
		HTTPClient:          httpClient,
		DisableStandaloneSSE: true,
	}

	// Create MCP client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "file-exchange",
		Version: "1.0.0",
	}, nil)

	// Connect to the MCP server
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to MCP server: %w", err)
	}

	b.session = session
	return nil
}

func (b *FsMcpBackend) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	if err := b.ensureConnection(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)
	b.debugLog("ListDir: path=%s, resolved=%s", path, resolvedPath)

	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_directory",
		Arguments: map[string]any{
			"path": resolvedPath,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list directory: %w", err)
	}

	if len(result.Content) == 0 {
		return []FileInfo{}, nil
	}

	// Parse the text content into FileInfo array
	var files []FileInfo
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			if err := json.Unmarshal([]byte(textContent.Text), &files); err != nil {
				return nil, fmt.Errorf("failed to parse file list: %w", err)
			}
			break
		}
	}

	return files, nil
}

func (b *FsMcpBackend) Read(ctx context.Context, path string) ([]byte, error) {
	if err := b.ensureConnection(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)
	b.debugLog("Read: path=%s, resolved=%s", path, resolvedPath)

	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_file",
		Arguments: map[string]any{
			"path": resolvedPath,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	if len(result.Content) == 0 {
		return nil, ErrNotFound
	}

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			return []byte(textContent.Text), nil
		}
	}

	return nil, ErrNotFound
}

func (b *FsMcpBackend) Write(ctx context.Context, path string, content []byte) error {
	if err := b.ensureConnection(ctx); err != nil {
		return err
	}

	resolvedPath := b.resolvePath(path)
	b.debugLog("Write: original_path=%s, resolved_path=%s, content_len=%d", path, resolvedPath, len(content))

	// Ensure parent directory exists
	parentDir := filepath.Dir(resolvedPath)
	if parentDir != "/" && parentDir != "" {
		b.debugLog("Creating parent directory: %s", parentDir)
		_, dirErr := b.session.CallTool(ctx, &mcp.CallToolParams{
			Name: "create_directory",
			Arguments: map[string]any{
				"path": parentDir,
			},
		})
		// Ignore error - directory might already exist
		_ = dirErr
	}

	// Try write_file first
	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"path":    resolvedPath,
			"content": string(content),
		},
	})
	if err != nil {
		b.debugLog("Write failed: %v", err)
		return fmt.Errorf("failed to write file: %w", err)
	}

	b.debugLog("Write result: isError=%v, content=%v", result.IsError, result.Content)
	if result.IsError {
		// Extract error message from content
		for _, content := range result.Content {
			if textContent, ok := content.(*mcp.TextContent); ok {
				return fmt.Errorf("MCP server error: %s", textContent.Text)
			}
		}
		return fmt.Errorf("MCP server returned error")
	}
	return nil
}

func (b *FsMcpBackend) Delete(ctx context.Context, path string) error {
	if err := b.ensureConnection(ctx); err != nil {
		return err
	}

	resolvedPath := b.resolvePath(path)

	_, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "delete_file",
		Arguments: map[string]any{
			"path": resolvedPath,
		},
	})
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

func (b *FsMcpBackend) Stat(ctx context.Context, path string) (interface{}, error) {
	if err := b.ensureConnection(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)

	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_file_info",
		Arguments: map[string]any{
			"path": resolvedPath,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	if len(result.Content) == 0 {
		return nil, ErrNotFound
	}

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			return textContent.Text, nil
		}
	}

	return nil, ErrNotFound
}

// Ensure fs_mcp backend is registered
func init() {
	RegisterBackend("fs-mcp", NewFsMcpBackend)
}