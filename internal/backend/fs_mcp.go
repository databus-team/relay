package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/user/relay/internal/exchange"
)

type FsMcpBackend struct {
	url        string
	remoteRoot string
	commandDir string
	headers    map[string]string
	session    *mcp.ClientSession
	exchanger  *exchange.FileExchange
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

	commandDir := "/tmp/relay-commands"
	if dir, ok := config["command_dir"].(string); ok && dir != "" {
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
		// Convert Windows-style absolute paths to Unix
		return strings.ReplaceAll(path, "\\", "/")
	}
	// Use Unix-style paths for remote filesystem
	joined := filepath.Join(b.remoteRoot, path)
	return strings.ReplaceAll(joined, "\\", "/")
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
	if debugMode && err == nil && resp != nil {
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
		Endpoint:             b.url,
		HTTPClient:           httpClient,
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

// ListDir regex patterns for parsing text output
var (
	listDirPattern    = regexp.MustCompile(`\[FILE\]\s+(.+?)\s+\(file://(.+?)\)\s+-\s+(\d+)\s+bytes`)
	listDirDirPattern = regexp.MustCompile(`\[DIR\]\s+(.+?)\s+\(file://(.+?)\)`)
)

// treeNode represents a node in the tree JSON structure
type treeNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Type     string     `json:"type"`
	Modified string     `json:"modified,omitempty"`
	Size     int64      `json:"size,omitempty"`
	Children []treeNode `json:"children,omitempty"`
}

func (b *FsMcpBackend) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	if err := b.ensureConnection(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)
	b.debugLog("ListDir: path=%s, resolved=%s", path, resolvedPath)

	// First try tree command for structured output with modified times
	files, err := b.listDirTree(ctx, resolvedPath)
	if err == nil && len(files) > 0 {
		return files, nil
	}
	b.debugLog("tree failed: %v, falling back to list_directory", err)

	// Fallback to list_directory
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

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			// Try JSON first
			if err := json.Unmarshal([]byte(textContent.Text), &files); err == nil && len(files) > 0 {
				return files, nil
			}

			// Parse text format: [FILE] name (file://path) - N bytes
			files = parseListOutput(textContent.Text)
			if len(files) > 0 {
				return files, nil
			}

			return nil, fmt.Errorf("failed to parse directory listing (raw: %s)", textContent.Text)
		}
	}

	return files, nil
}

func (b *FsMcpBackend) listDirTree(ctx context.Context, path string) ([]FileInfo, error) {
	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "tree",
		Arguments: map[string]any{
			"path":  path,
			"depth": 1, // Only need one level for list
		},
	})
	if err != nil {
		return nil, err
	}

	if len(result.Content) == 0 {
		return nil, fmt.Errorf("empty response")
	}

	for _, content := range result.Content {
		// Try TextContent first (text format)
		if textContent, ok := content.(*mcp.TextContent); ok {
			// Extract JSON from text (may have prefix like "Directory tree for...")
			files := parseTreeText(textContent.Text, path)
			if len(files) > 0 {
				return files, nil
			}
		}
	}

	return nil, fmt.Errorf("failed to parse tree output")
}

func parseTreeText(text string, parentPath string) []FileInfo {
	// Try to extract JSON object from text
	jsonStart := strings.Index(text, "{")
	if jsonStart == -1 {
		return nil
	}
	jsonText := text[jsonStart:]
	return parseTreeJSON([]byte(jsonText), parentPath)
}

func parseTreeJSON(data []byte, parentPath string) []FileInfo {
	var root treeNode
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}

	var files []FileInfo
	for _, child := range root.Children {
		files = append(files, treeNodeToFileInfo(child))
	}
	return files
}

func treeNodeToFileInfo(node treeNode) FileInfo {
	return FileInfo{
		Name:    node.Name,
		IsDir:   node.Type == "directory",
		Size:    node.Size,
		ModTime: node.Modified,
	}
}

// parseListOutput parses MCP text output format to FileInfo slice
func parseListOutput(text string) []FileInfo {
	var files []FileInfo

	// Parse files: [FILE] name (file://path) - N bytes
	for _, match := range listDirPattern.FindAllStringSubmatch(text, -1) {
		size, _ := strconv.ParseInt(match[3], 10, 64)
		files = append(files, FileInfo{
			Name:    strings.TrimSpace(match[1]),
			IsDir:   false,
			Size:    size,
			ModTime: "",
		})
	}

	// Parse directories: [DIR] name (file://path)
	for _, match := range listDirDirPattern.FindAllStringSubmatch(text, -1) {
		files = append(files, FileInfo{
			Name:    strings.TrimSpace(match[1]),
			IsDir:   true,
			Size:    0,
			ModTime: "",
		})
	}

	return files
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
	// Convert to forward slashes for remote filesystem
	parentDir = strings.ReplaceAll(parentDir, "\\", "/")
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

func (b *FsMcpBackend) Exec(ctx context.Context, cmd string, cwd string, timeout int) (string, error) {
	if err := b.ensureConnection(ctx); err != nil {
		return "", err
	}

	if b.exchanger == nil {
		b.exchanger = exchange.NewFileExchange(b, b.commandDir)
	}

	result, err := b.exchanger.ExecuteCommand(ctx, cmd, cwd, timeout)
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("command failed with exit code %d: %s", result.ExitCode, result.Stderr)
	}

	return result.Stdout, nil
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

func (b *FsMcpBackend) Ping(ctx context.Context, commandDir, watchID string) error {
	if err := b.ensureConnection(ctx); err != nil {
		return err
	}

	// Heartbeat file is stored in commandDir
	heartbeatPath := commandDir + "/.heartbeat"
	data, err := b.Read(ctx, heartbeatPath)
	if err != nil {
		return fmt.Errorf("remote watcher appears to be offline (heartbeat file not found)")
	}

	// Parse timestamp from heartbeat file
	timestamp := strings.TrimSpace(string(data))
	heartbeatTime, parseErr := time.Parse(time.RFC3339, timestamp)
	if parseErr != nil {
		return fmt.Errorf("remote watcher appears to be offline (invalid heartbeat format)")
	}

	// Check if heartbeat is recent (within 15 seconds)
	age := time.Since(heartbeatTime)
	if age > 15*time.Second {
		return fmt.Errorf("remote watcher appears to be offline (heartbeat age: %v)", age.Round(time.Second))
	}

	return nil
}

// Ensure fs_mcp backend is registered
func init() {
	RegisterBackend("fs-mcp", NewFsMcpBackend)
}
