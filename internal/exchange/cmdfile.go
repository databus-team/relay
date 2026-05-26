package exchange

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// FileBackend defines the interface for file operations required by FileExchange
type FileBackend interface {
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, content []byte) error
	Delete(ctx context.Context, path string) error
}

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
	backend      FileBackend
	commandDir   string
	pollDefault  int // seconds
	pollInterval int // seconds
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func NewFileExchange(backend FileBackend, commandDir string) *FileExchange {
	return &FileExchange{
		backend:      backend,
		commandDir:   commandDir,
		pollDefault:  30,
		pollInterval: 2,
	}
}

func (e *FileExchange) SetPollInterval(interval int) {
	e.pollInterval = interval
}

func (e *FileExchange) SetTimeout(timeout int) {
	e.pollDefault = timeout
}

func (e *FileExchange) ExecuteCommand(ctx context.Context, cmd, cwd string, timeout int) (*ResultFile, error) {
	cmdID := newUUID()

	cmdFile := CmdFile{
		ID:      cmdID,
		Cmd:     cmd,
		Cwd:     cwd,
		Timeout: timeout,
	}

	cmdData, err := json.Marshal(cmdFile)
	if err != nil {
		return nil, err
	}

	cmdPath := "cmd-" + cmdID + ".json"
	if err := e.backend.Write(ctx, e.commandDir+"/"+cmdPath, cmdData); err != nil {
		return nil, err
	}

	result, err := e.pollResult(ctx, cmdID)
	if err != nil {
		return nil, err
	}

	// Cleanup - ignore errors
	_ = e.backend.Delete(ctx, e.commandDir+"/"+cmdPath)
	_ = e.backend.Delete(ctx, e.commandDir+"/result-"+cmdID+".json")

	return result, nil
}

func (e *FileExchange) pollResult(ctx context.Context, cmdID string) (*ResultFile, error) {
	resultPath := "result-" + cmdID + ".json"
	pollInterval := time.Duration(e.pollInterval) * time.Second
	timeout := time.Duration(e.pollDefault) * time.Second

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			data, err := e.backend.Read(ctx, e.commandDir+"/"+resultPath)
			if err != nil {
				continue
			}

			var result ResultFile
			if err := json.Unmarshal(data, &result); err != nil {
				continue
			}

			if result.ID != cmdID {
				continue
			}

			return &result, nil
		}
	}
}