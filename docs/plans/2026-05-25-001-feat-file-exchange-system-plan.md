---
title: "Generic File Exchange Command Execution System"
type: feat
status: active
date: 2026-05-25
origin: docs/brainstorms/patch-transfer-system.md
---

# Generic File Exchange Command Execution System

## Summary

A Go-based bidirectional file exchange system where a single node can push files to or pull files from a remote server, with configurable trigger actions (shell scripts) executed on the receiving end. The system generalizes beyond patch transfer to support arbitrary file-driven command execution across network boundaries.

---

## Problem Frame

Internal network developers need to execute commands on air-gap machines where direct network access is unavailable. A file-based command exchange protocol allows sending commands (via JSON files) to the remote end, which executes them and returns results — all through a shared filesystem (fs-mcp) or file server (JumpServer). Unlike the original patch-transfer design, this is a general-purpose tool that can be configured for any file-triggered workflow.

---

## Requirements

- R1. Single Go binary acts as both sender and receiver (bidirectional node)
- R2. Configuration in YAML at `~/.file-exchange/config.yaml`, overridable via `--config` flag
- R3. Backend abstraction: `fs-mcp`, `jumpserver`, `local` — switchable via config
- R4. File transfer operations: `list_dir`, `read`, `write`, `delete` via backend interface
- R5. Watcher mode: poll remote directory for files matching glob patterns, trigger actions
- R6. Action types: `exec` (shell command via file exchange), `file_delete` (remove remote file)
- R7. File exchange protocol: write `cmd-{uuid}.json` → poll `result-{uuid}.json` → delete both
- R8. Auth: HTTP proxy cookie interception for fs-mcp/jumpserver; local backend needs no auth
- R9. Token caching to `token_cache_file` with expiry; interactive login when expired
- R10. Action `if` conditions: `jobs.<id>.success` / `jobs.<id>.failure` references
- R11. Built-in variables in action fields: `{file_path}`, `{file_name}`, `{file_dir}`, `{timestamp}`
- R12. On action failure, remaining actions skipped; remote file left untouched
- R13. On all required actions success, remote file deleted (unless `keep_file: true`)
- R14. Jumpserver backend does NOT support `exec` action type (only file operations)

**Origin actors:** A1 (Developer/Operator)
**Origin acceptance examples:** AE1, AE2, AE3, AE5 (adapted from patch-transfer)

---

## Scope Boundaries

- Notification on action failure (Slack/email)
- Automated retry on action failure
- Persistent job state across restarts
- Webhook-based push notifications (vs polling)

### Deferred for later
- Push mode implementation (separate PR)
- Multiple simultaneous watchers
- Action timeout per-action configuration
- trigger-ci command for CI pipeline triggering (origin R39-R41)

### Outside this product's identity
- Direct MCP integration (file exchange only, not MCP calls)
- Server-side modifications (callback endpoints, token pages)

---

## Context & Research

### Relevant Code and Patterns

- `debug.py` — Reference for fs-mcp connection with cookie auth (Python, but the cookie structure is Go-relevant)
- `docs/brainstorms/patch-transfer-system.md` — Origin design with sequence diagrams and file formats

### Institutional Learnings

- No prior implementations in this repo; greenfield project
- Backend abstraction is critical for swappable storage backends

### External References

- fs-mcp: Model Context Protocol filesystem server (streamable-http transport)
- JumpServer REST API: upload/download-only operations

---

## Key Technical Decisions

- **Go instead of Python**: User-specified language; better cross-compilation for target machines
- **Single binary, mode-based**: `file-exchange watch` vs `file-exchange push` via subcommands
- **Shell script responder**: command-responder implemented as shell script polling `command_dir`, not Go service — simpler for air-gap deployment
- **File exchange protocol preserved**: Even though the responder is shell, the file format (cmd-{uuid}.json / result-{uuid}.json) remains the same
- **Jumpserver exec blocked**: Document that `exec` action requires fs-mcp or local backend

---

## Open Questions

### Resolved During Planning

- **MCP call syntax for exec**: Using shell command strings (e.g., `cicd-mcp.create-branch ...`) passed to responder's shell — format is user-defined in config, not enforced by the tool.

### Deferred to Implementation

- Exact polling interval for command result (default: 2 seconds, configurable)
- Shell responder's error handling strategy for non-zero exit codes
- Config validation: how to fail fast if jumpserver backend uses `exec` action type

---

## Output Structure

```
file-exchange/
├── cmd/
│   └── file-exchange/
│       └── main.go           # CLI entry point
├── internal/
│   ├── backend/
│   │   ├── backend.go        # Backend interface
│   │   ├── fs_mcp.go        # fs-mcp backend
│   │   ├── jumpserver.go    # jumpserver backend
│   │   └── local.go         # local filesystem backend
│   ├── config/
│   │   └── config.go        # YAML config parsing
│   ├── auth/
│   │   └── proxy.go         # HTTP proxy auth flow
│   ├── watcher/
│   │   └── watcher.go       # File polling + action execution
│   └── exchange/
│       └── cmdfile.go       # cmd-{uuid}.json / result-{uuid}.json handling
├── scripts/
│   └── command-responder.sh # Shell script for air-gap machine
├── config.example.yaml      # Example configuration
├── go.mod
└── go.sum
```

---

## High-Level Technical Design

> *This illustrates the intended approach and is directional guidance for review, not implementation specification.*

### Architecture Overview

```
┌─────────────────┐         ┌─────────────────┐
│  file-exchange  │         │  Remote Server  │
│   (Go binary)   │         │   (fs-mcp/jump)  │
│                 │         │                 │
│  watch mode:    │  list   │  /patches/      │
│  ┌───────────┐  │◄───────►│  /commands/     │
│  │  Watcher  │  │         │                 │
│  └───────────┘  │         └────────┬────────┘
│        │       │  write/read           │
│        │       │         ┌─────────────▼────────┐
│        ▼       │         │  Air-Gap Machine      │
│  ┌───────────┐ │         │                       │
│  │  Backend  │ │  poll   │  command-responder.sh │
│  └───────────┘ │◄────────│  (shell script)       │
│                │ result  │                       │
└────────────────┘         └───────────────────────┘
```

### File Exchange Protocol (exec action)

```
1. Watcher writes cmd-{uuid}.json to command_dir via backend
2. Responder (shell) polls command_dir, finds new cmd file
3. Responder executes cmd string in cwd, writes result-{uuid}.json
4. Watcher polls for result-{uuid}.json until timeout or found
5. Watcher reads result, deletes both cmd and result files
```

### Config Format

```yaml
name: file-exchange
version: 1

backend:
  type: fs-mcp  # or jumpserver, local
  config:
    url: $FS_MCP_URL
    transport: streamable-http
    headers:
      Cookie: $FS_MCP_COOKIE
    command_dir: /commands  # for exec action

auth:
  method: proxy
  login_url: $LOGIN_URL
  token_cookie_name: code-server-session
  proxy_port: 8080
  token_cache_file: ~/.file-exchange/token.json

watchers:
  - name: Apply Patch
    on:
      file_match: "*.patch"
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
        cwd: /path/to/repo
      - id: cleanup
        type: file_delete
        path: {file_remote_path}

interval_seconds: 60
```

---

## Implementation Units

### U1. Project Scaffolding

**Goal:** Create Go module structure with basic CLI entry point

**Requirements:** R1, R2

**Dependencies:** None

**Files:**
- Create: `go.mod`
- Create: `cmd/file-exchange/main.go`
- Create: `config.example.yaml`

**Approach:**
- Initialize Go module
- Add `watch` subcommand structure (`push` deferred)
- Basic flag parsing (`--config`)
- Print version / help

**Patterns to follow:**
- Standard Go CLI structure (kingpin or cobra for flags)

**Test scenarios:**
- Happy path: `./file-exchange --help` shows usage
- Happy path: `./file-exchange watch --help` shows watch-specific help
- Error path: Invalid `--config` path returns error

**Verification:**
- Binary builds successfully: `go build -o file-exchange ./cmd/file-exchange`
- `./file-exchange --help` displays CLI help

---

### U2. Backend Abstraction Layer

**Goal:** Define and implement `FileTransferBackend` interface with three backends

**Requirements:** R3, R4, R14

**Dependencies:** U1

**Files:**
- Create: `internal/backend/backend.go` (interface definition)
- Create: `internal/backend/fs_mcp.go` (fs-mcp implementation)
- Create: `internal/backend/jumpserver.go` (jumpserver implementation)
- Create: `internal/backend/local.go` (local filesystem implementation)
- Create: `internal/backend/fs_mcp_test.go`
- Create: `internal/backend/local_test.go`

**Approach:**
- Define `FileTransferBackend` interface with `ListDir`, `Read`, `Write`, `Delete`
- `FsMcpBackend`: uses fastmcp library (or raw HTTP with MCP protocol)
- `JumpServerBackend`: REST API calls for upload/download
- `LocalBackend`: direct filesystem operations (os.DirFS)
- Backend factory function based on config `type` field
- Document that `exec` action is not supported on JumpServer backend
- `SupportsExec()` returns a static value per backend type (JumpServer=false, fs-mcp/local=true). Config validation should fail at startup if JumpServer backend is configured with `exec` action type.

**Technical design:**
```go
type FileTransferBackend interface {
    ListDir(ctx context.Context, path string) ([]FileInfo, error)
    Read(ctx context.Context, path string) ([]byte, error)
    Write(ctx context.Context, path string, content []byte) error
    Delete(ctx context.Context, path string) error
    // SupportsExec returns true if this backend supports exec action type
    SupportsExec() bool
    // GetCommandDir returns the directory for command exchange
    GetCommandDir() string
}
```

**Patterns to follow:**
- Interface-based dependency injection for testability
- fs-mcp client patterns from debug.py (adapted to Go)

**Test scenarios:**
- Happy path: LocalBackend ListDir returns file list
- Happy path: LocalBackend Write then Read returns same content
- Edge case: LocalBackend Read nonexistent file returns error
- Error path: Write to read-only location returns error
- Integration: FsMcpBackend (if test environment available)

**Verification:**
- All backend implementations satisfy the interface
- Unit tests pass for LocalBackend

---

### U3. Configuration Parsing

**Goal:** Parse YAML config into typed Go structures

**Requirements:** R2, R5, R6, R10, R11, R12, R13

**Dependencies:** U1

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

**Approach:**
- Define `Config` struct matching the YAML schema
- Environment variable expansion support (e.g., `$FS_MCP_URL`)
- Watcher config with `on.file_match` glob pattern
- Action types: `exec`, `file_delete`
- Action `if` condition parsing
- Built-in variable extraction from action fields

**Technical design:**
```go
type Config struct {
    Name     string `yaml:"name"`
    Version  int    `yaml:"version"`
    Backend  BackendConfig  `yaml:"backend"`
    Auth     AuthConfig     `yaml:"auth"`
    Watchers []WatcherConfig `yaml:"watchers"`
    Interval int            `yaml:"interval_seconds"`
}

type WatcherConfig struct {
    Name string         `yaml:"name"`
    On   OnConfig       `yaml:"on"`
    Jobs []JobConfig    `yaml:"jobs"`
}

type JobConfig struct {
    ID       string      `yaml:"id"`
    Name     string      `yaml:"name"`
    Type     string      `yaml:"type"`  // exec, file_delete
    Cmd      string      `yaml:"cmd"`
    Cwd      string      `yaml:"cwd"`
    Path     string      `yaml:"path"`
    If       string      `yaml:"if"`
    KeepFile bool        `yaml:"keep_file"`  // if true, do not delete remote file after success
}
```

**Test scenarios:**
- Happy path: Parse valid YAML config
- Edge case: Missing required fields return error
- Edge case: Unknown backend type returns error
- Edge case: Unknown action type returns error

**Verification:**
- `config.example.yaml` parses without error
- Unit tests pass

---

### U4. Auth Flow (HTTP Proxy Cookie Interception)

**Goal:** Implement interactive token acquisition via local HTTP proxy

**Requirements:** R8, R9

**Dependencies:** U1

**Files:**
- Create: `internal/auth/proxy.go`
- Create: `internal/auth/proxy_test.go`

**Approach:**
- Start local HTTP proxy on configured port
- Set `HTTP_PROXY` env var before opening browser
- Proxy intercepts `Set-Cookie` headers, extracts configured cookie name
- Save token to `token_cache_file` with expiry timestamp
- 5-minute timeout on auth flow
- Fallback: manual token paste if proxy interception fails

**Technical design:**
- Use `net/http/httputil` for proxy functionality
- `webbrowser.Open()` equivalent via `os.StartProcess` (open URL)
- Token cache format: JSON with `token`, `expires_at` fields

**Patterns to follow:**
- Proxy pattern from brainstorm (F3 Auth flow)

**Test scenarios:**
- Edge case: Proxy start on already-in-use port
- Edge case: Auth timeout after 5 minutes
- Edge case: Invalid token cookie name (not found in response)
- Happy path: Token saved to cache file

**Verification:**
- Auth flow can be triggered when token is missing/expired

---

### U5. File Exchange Protocol (cmd/result handling)

**Goal:** Implement command file write and result polling

**Requirements:** R7

**Dependencies:** U2

**Files:**
- Create: `internal/exchange/cmdfile.go`
- Create: `internal/exchange/cmdfile_test.go`

**Approach:**
- Generate UUID for command correlation
- Write `cmd-{uuid}.json` with command details
- Poll for `result-{uuid}.json` with configurable interval and timeout
- Parse result: exit_code, stdout, stderr, completed_at
- Delete both files after reading result

**Technical design:**
```go
type CmdFile struct {
    ID       string `json:"id"`
    Cmd      string `json:"cmd"`
    Cwd      string `json:"cwd"`
    Timeout  int    `json:"timeout"`
}

type ResultFile struct {
    ID          string `json:"id"`
    ExitCode    int    `json:"exit_code"`
    Stdout      string `json:"stdout"`
    Stderr      string `json:"stderr"`
    CompletedAt string `json:"completed_at"`
}
```

**Test scenarios:**
- Happy path: Write cmd file, poll and read result
- Edge case: Result file not found within timeout
- Edge case: Malformed result JSON
- Edge case: Result ID doesn't match cmd ID

**Verification:**
- cmd-{uuid}.json written to command_dir
- result-{uuid}.json read correctly
- Both files deleted after successful exchange

---

### U6. Watcher Implementation

**Goal:** Poll remote directory and execute configured actions

**Requirements:** R5, R6, R10, R11, R12, R13

**Dependencies:** U2, U3, U5

**Files:**
- Create: `internal/watcher/watcher.go`
- Create: `internal/watcher/watcher_test.go`

**Approach:**
- Loop with `interval_seconds` sleep
- `ListDir` on configured watch directories
- Match files against watcher `on.file_match` glob patterns
- For matched files, execute jobs in order
- Evaluate `if` conditions before each job
- Substitute built-in variables in job fields
- On job failure: stop processing, leave file
- On all jobs success: delete remote file (unless `keep_file`)
- Daemon mode (default), `--once` flag for single iteration

**Patterns to follow:**
- GitHub Actions workflow execution model

**Test scenarios:**
- Happy path: File matches glob, action executed successfully
- Edge case: No files match glob pattern
- Edge case: Action fails, file not deleted
- Edge case: `if` condition evaluates to false, job skipped
- Edge case: Variable substitution in cmd string
- Error path: Backend returns error during ListDir

**Verification:**
- Watcher processes new files matching pattern
- Failed actions leave file intact
- Successful actions delete file (unless configured otherwise)

---

### U7. Shell Command Responder (Air-Gap Machine)

**Goal:** Provide a shell script that polls command_dir and executes commands

**Requirements:** R7

**Dependencies:** None (standalone shell script; file format defined by R7)

**Files:**
- Create: `scripts/command-responder.sh`

**Approach:**
- Polling loop with configurable interval (default: 2s)
- Find `cmd-*.json` files in command_dir
- Read cmd field, execute via shell in specified cwd
- Capture exit_code, stdout, stderr
- Write `result-{uuid}.json` with completion timestamp
- Delete `cmd-*.json` after writing result
- Log errors to stderr

**Technical design:**
```bash
#!/bin/bash
COMMAND_DIR="/commands"
POLL_INTERVAL=2

while true; do
    for cmd_file in "$COMMAND_DIR"/cmd-*.json; do
        [ -e "$cmd_file" ] || continue
        # Parse JSON, execute cmd, write result
    done
    sleep "$POLL_INTERVAL"
done
```

**Test scenarios:**
- Happy path: Command executed, result file written
- Edge case: Command times out
- Edge case: Malformed cmd JSON
- Edge case: Working directory doesn't exist

**Verification:**
- Script runs as daemon
- Responds to cmd files within POLL_INTERVAL

---

## System-Wide Impact

- **Backend swap**: Changing `backend.type` in config changes all file operations — no code changes needed
- **Error propagation**: Backend errors propagate to watcher, which logs and continues polling
- **State lifecycle**: No persistent state; in-memory tracking of processed files only
- **Integration coverage**: End-to-end test requires fs-mcp or local backend with responder script

---

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| fs-mcp library availability in Go | Use raw HTTP with MCP protocol if no Go SDK exists; research during U2 |
| JumpServer API auth complexity | Focus on upload/download only; exec not supported |
| Responder script portability | Use POSIX-compliant shell; test on target air-gap OS |
| Token expiry during long operations | Check token validity before each operation |
| Command injection via unsanitized file names | Validate file names at upload (whitelist alphanumeric, dash, dot, underscore); escape variables in shell commands |
| Token storage in plaintext JSON | Use OS credential manager (Keychain/secretservice) if available; set restrictive file permissions (0o600) if local file required |
| HTTP_PROXY env var leakage | Scope HTTP_PROXY only to browser subprocess, not global process environment |

---

## Documentation / Operational Notes

- Installation: `go install` or release binary
- Configuration: Copy `config.example.yaml` to `~/.file-exchange/config.yaml`
- Responder deployment: Copy `command-responder.sh` to air-gap machine, run as systemd service
- First run: Auth flow triggers automatically if token missing/expired

---

## Sources & References

- **Origin document:** [docs/brainstorms/patch-transfer-system.md](docs/brainstorms/patch-transfer-system.md)
- Reference code: [debug.py](debug.py) — fs-mcp connection pattern