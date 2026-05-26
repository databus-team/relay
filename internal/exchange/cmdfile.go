package exchange

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
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
	// Op is the operation type for built-in commands. When Op is set,
	// the Cmd/Cwd/Timeout fields may be empty and the operation is handled
	// internally (e.g., "relay:config-sync" for config hot reload).
	Op      string `json:"op,omitempty"`
	// Payload holds base64-encoded content for built-in commands.
	Payload string `json:"payload,omitempty"`
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
		pollDefault:  300, // default to 300s to match responder default
		pollInterval: 2,
	}
}

func (e *FileExchange) SetPollInterval(interval int) {
	e.pollInterval = interval
}

func (e *FileExchange) SetTimeout(timeout int) {
	e.pollDefault = timeout
}

// ConfigSyncOp is the operation identifier for config hot reload.
const ConfigSyncOp = "relay:config-sync"

// BuildConfigSyncCmd creates a CmdFile for config sync with base64-encoded payload.
func BuildConfigSyncCmd(payload []byte) CmdFile {
	return CmdFile{
		ID:      newUUID(),
		Op:      ConfigSyncOp,
		Payload: base64.StdEncoding.EncodeToString(payload),
	}
}

func (e *FileExchange) ExecuteCommand(ctx context.Context, cmd, cwd string, timeout int) (*ResultFile, error) {
	cmdID := newUUID()
	cmdPath := "cmd-" + cmdID + ".json"
	fullCmdPath := e.commandDir + "/" + cmdPath

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

	log.Printf("[exchange] Writing cmd file: %s", fullCmdPath)
	if err := e.backend.Write(ctx, fullCmdPath, cmdData); err != nil {
		return nil, err
	}

	// Use provided timeout if set, otherwise fallback to default
	pollTimeout := e.pollDefault
	if timeout > 0 {
		pollTimeout = timeout
	}

	result, err := e.pollResultWithTimeout(ctx, cmdID, pollTimeout)
	if err != nil {
		return nil, err
	}

	// Cleanup - ignore errors
	log.Printf("[exchange] Cleaning up: %s and result-%s.json", cmdPath, cmdID)
	_ = e.backend.Delete(ctx, fullCmdPath)
	_ = e.backend.Delete(ctx, e.commandDir+"/result-"+cmdID+".json")

	return result, nil
}

func (e *FileExchange) pollResultWithTimeout(ctx context.Context, cmdID string, timeoutSeconds int) (*ResultFile, error) {
	resultPath := "result-" + cmdID + ".json"
	fullPath := e.commandDir + "/" + resultPath
	pollInterval := time.Duration(e.pollInterval) * time.Second
	var timeout time.Duration
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	} else {
		timeout = time.Duration(e.pollDefault) * time.Second
	}

	log.Printf("[exchange] Polling for result at %s (timeout: %v)", fullPath, timeout)

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
			data, err := e.backend.Read(ctx, fullPath)
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
