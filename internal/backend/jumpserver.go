package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type JumpServerBackend struct {
	baseURL    string
	remoteRoot string
	username   string
	password   string
	token      string
	commandDir string
	httpClient *http.Client
}

type JumpServerConfig struct {
	BaseURL    string `mapstructure:"base_url"`
	Username   string `mapstructure:"username"`
	Password   string `mapstructure:"password"`
	Token      string `mapstructure:"token"`
	CommandDir string `mapstructure:"command_dir"`
	RemoteRoot string `mapstructure:"remote_root"`
}

func NewJumpServerBackend(config map[string]interface{}) (FileTransferBackend, error) {
	baseURL, _ := config["base_url"].(string)
	if baseURL == "" {
		return nil, fmt.Errorf("jumpserver backend requires 'base_url' config")
	}

	// Remove trailing slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	commandDir := "/commands"
	if dir, ok := config["command_dir"].(string); ok {
		commandDir = dir
	}

	remoteRoot := "/"
	if root, ok := config["remote_root"].(string); ok {
		remoteRoot = root
	}

	username, _ := config["username"].(string)
	password, _ := config["password"].(string)
	token, _ := config["token"].(string)

	return &JumpServerBackend{
		baseURL:    baseURL,
		username:   username,
		password:   password,
		token:      token,
		remoteRoot: remoteRoot,
		commandDir: commandDir,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (b *JumpServerBackend) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(b.remoteRoot, path)
}

func (b *JumpServerBackend) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	if err := b.ensureAuth(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)

	url := fmt.Sprintf("%s/api/v1/filesystem/list/", b.baseURL)
	reqBody := map[string]string{
		"path": resolvedPath,
	}

	body, err := b.doRequest(ctx, "POST", url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to list directory: %w", err)
	}

	var result struct {
		Data []FileInfo `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result.Data, nil
}

func (b *JumpServerBackend) Read(ctx context.Context, path string) ([]byte, error) {
	if err := b.ensureAuth(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)

	url := fmt.Sprintf("%s/api/v1/filesystem/read/", b.baseURL)
	reqBody := map[string]string{
		"path": resolvedPath,
	}

	body, err := b.doRequest(ctx, "POST", url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var result struct {
		Data string `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return []byte(result.Data), nil
}

func (b *JumpServerBackend) Write(ctx context.Context, path string, content []byte) error {
	if err := b.ensureAuth(ctx); err != nil {
		return err
	}

	resolvedPath := b.resolvePath(path)

	url := fmt.Sprintf("%s/api/v1/filesystem/write/", b.baseURL)
	reqBody := map[string]interface{}{
		"path":    resolvedPath,
		"content": string(content),
	}

	_, err := b.doRequest(ctx, "POST", url, reqBody)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func (b *JumpServerBackend) Delete(ctx context.Context, path string) error {
	if err := b.ensureAuth(ctx); err != nil {
		return err
	}

	resolvedPath := b.resolvePath(path)

	url := fmt.Sprintf("%s/api/v1/filesystem/delete/", b.baseURL)
	reqBody := map[string]string{
		"path": resolvedPath,
	}

	_, err := b.doRequest(ctx, "POST", url, reqBody)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// SupportsExec returns false - JumpServer backend does NOT support exec action
func (b *JumpServerBackend) SupportsExec() bool {
	return false
}

func (b *JumpServerBackend) Exec(ctx context.Context, cmd string, cwd string) (string, error) {
	return "", ErrNotSupported
}

func (b *JumpServerBackend) ensureAuth(ctx context.Context) error {
	if b.token != "" {
		return nil
	}

	if b.username == "" || b.password == "" {
		return fmt.Errorf("jumpserver backend requires 'username' and 'password' (or 'token') config")
	}

	url := fmt.Sprintf("%s/api/v1/auth/login/", b.baseURL)
	reqBody := map[string]string{
		"username": b.username,
		"password": b.password,
	}

	resp, err := b.doRequestRaw(ctx, "POST", url, reqBody)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	var result struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("failed to parse auth response: %w", err)
	}

	if result.Token == "" {
		return fmt.Errorf("authentication failed: no token received")
	}

	b.token = result.Token
	return nil
}

func (b *JumpServerBackend) doRequest(ctx context.Context, method string, url string, reqBody interface{}) ([]byte, error) {
	resp, err := b.doRequestRaw(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}

	// Check for error response
	var errResp struct {
		Detail string `json:"detail"`
	}

	if err := json.Unmarshal(resp, &errResp); err == nil && errResp.Detail != "" {
		return nil, fmt.Errorf("request failed: %s", errResp.Detail)
	}

	return resp, nil
}

func (b *JumpServerBackend) doRequestRaw(ctx context.Context, method string, url string, reqBody interface{}) ([]byte, error) {
	var body []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		body = b
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

func (b *JumpServerBackend) Stat(ctx context.Context, path string) (interface{}, error) {
	if err := b.ensureAuth(ctx); err != nil {
		return nil, err
	}

	resolvedPath := b.resolvePath(path)

	url := fmt.Sprintf("%s/api/v1/filesystem/stat/", b.baseURL)
	reqBody := map[string]string{
		"path": resolvedPath,
	}

	body, err := b.doRequest(ctx, "POST", url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	var result struct {
		Data interface{} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result.Data, nil
}

func (b *JumpServerBackend) Ping(ctx context.Context, commandDir, watchID string) error {
	return ErrNotSupported
}

// Ensure jumpserver backend is registered
func init() {
	RegisterBackend("jumpserver", NewJumpServerBackend)
}