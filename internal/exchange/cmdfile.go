package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/user/relay/internal/backend"
)

type CmdFile struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	Cwd     string `json:"cwd"`
	Timeout int    `json:"timeout"`
}

type ResultFile struct {
	ID          string `json:"id"`
	ExitCode    int    `json:"exit_code"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	CompletedAt string `json:"completed_at"`
}

type FileExchange struct {
	backend     backend.FileTransferBackend
	commandDir  string
	pollDefault time.Duration
	pollInterval time.Duration
}

func NewFileExchange(backend backend.FileTransferBackend, commandDir string) *FileExchange {
	return &FileExchange{
		backend:     backend,
		commandDir:  commandDir,
		pollDefault: 30 * time.Second,
		pollInterval: 2 * time.Second,
	}
}

func (e *FileExchange) SetPollInterval(interval time.Duration) {
	e.pollInterval = interval
}

func (e *FileExchange) SetTimeout(timeout time.Duration) {
	e.pollDefault = timeout
}

func (e *FileExchange) ExecuteCommand(ctx context.Context, cmd, cwd string, timeout int) (*ResultFile, error) {
	cmdID := uuid.New().String()

	cmdFile := CmdFile{
		ID:      cmdID,
		Cmd:     cmd,
		Cwd:     cwd,
		Timeout: timeout,
	}

	cmdData, err := json.Marshal(cmdFile)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal command: %w", err)
	}

	cmdPath := fmt.Sprintf("cmd-%s.json", cmdID)
	if err := e.backend.Write(ctx, e.commandDir+"/"+cmdPath, cmdData); err != nil {
		return nil, fmt.Errorf("failed to write command file: %w", err)
	}

	result, err := e.pollResult(ctx, cmdID)
	if err != nil {
		return nil, err
	}

	if err := e.backend.Delete(ctx, e.commandDir+"/"+cmdPath); err != nil {
		fmt.Fprintf(stderr, "Warning: failed to delete command file: %v\n", err)
	}

	resultPath := fmt.Sprintf("result-%s.json", cmdID)
	if err := e.backend.Delete(ctx, e.commandDir+"/"+resultPath); err != nil {
		fmt.Fprintf(stderr, "Warning: failed to delete result file: %v\n", err)
	}

	return result, nil
}

func (e *FileExchange) pollResult(ctx context.Context, cmdID string) (*ResultFile, error) {
	timeout := e.pollDefault
	if ctx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	resultPath := fmt.Sprintf("result-%s.json", cmdID)

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for result: %w", ctx.Err())
		case <-ticker.C:
			data, err := e.backend.Read(ctx, e.commandDir+"/"+resultPath)
			if err == backend.ErrNotFound {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to read result: %w", err)
			}

			var result ResultFile
			if err := json.Unmarshal(data, &result); err != nil {
				return nil, fmt.Errorf("failed to parse result: %w", err)
			}

			if result.ID != cmdID {
				return nil, fmt.Errorf("result ID mismatch: expected %s, got %s", cmdID, result.ID)
			}

			return &result, nil
		}
	}
}

var stderr = os.Stderr