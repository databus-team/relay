# PROJECT KNOWLEDGE BASE

**Generated:** 2026-05-26
**Commit:** d26a1ca
**Branch:** feat/file-exchange-system

## OVERVIEW

Go-based file exchange & remote command execution system. Monitors remote directories via pluggable backends, triggers exec/file_delete jobs on file pattern matches. Supports config hot-reload without restart.

Stack: Go 1.25, kingpin (CLI), MCP SDK, yaml.v3

## STRUCTURE

```
relay/
├── cmd/relay/          # Entry point: CLI commands (watch/push/pull/exec/sync/list/cleanup)
├── internal/
│   ├── backend/        # FileTransferBackend interface + 3 implementations
│   ├── watcher/        # Daemon: polling loop, config hot-reload, job execution
│   ├── exchange/       # CmdFile/ResultFile protocol (JSON over shared dir)
│   ├── config/         # YAML config loader with Windows path normalization
│   └── auth/           # HTTP proxy auth with token caching
├── scripts/            # Shell command-responder for remote exec
└── docs/               # Plans and brainstorms
```

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| CLI commands | `cmd/relay/main.go` | Each subcommand = one `runXxx()` func |
| Add a backend | `internal/backend/` | See `## internal/backend/` below |
| Watcher loop | `internal/watcher/watcher.go` | Run(), processWatch(), checkConfigSync() |
| Cmd protocol | `internal/exchange/cmdfile.go` | CmdFile, ResultFile, ConfigSyncOp |
| Config schema | `internal/config/config.go` | Config struct + WatchConfig/JobConfig |
| Auth proxy | `internal/auth/proxy.go` | Browser-based login, cookie extraction |

## CONVENTIONS

- **Standard Go layout**: `cmd/` for binary, `internal/` for packages
- **Error handling**: `fmt.Fprintf(os.Stderr, ...)` + `os.Exit(1)` in main; errors returned from internal
- **Testing**: Standard `testing` package, `_test.go` beside source, table-driven tests
- **Dependency injection**: Backend factory pattern via `RegisterBackend()`/`init()`
- **Config format**: YAML with `yaml:` struct tags, env var expansion (`$VAR`)
- **Windows compat**: MSYS-style path normalization (`/d/...` → `D:\...`)

## ANTI-PATTERNS

- **No global state** beyond the backend registry map (`backends` var in backend.go)
- **No `as any`/`@ts-ignore`** — this is Go, type-safe
- **No framework** beyond standard lib + kingpin + MCP SDK
- **No ORM** — no database layer

## internal/backend/

Pluggable file transfer backends with factory pattern.

### WHERE TO LOOK

| File | Role |
|------|------|
| `backend.go` | `FileTransferBackend` interface (7 methods), `RegisterBackend()`/`NewBackend()` |
| `local.go` | Local filesystem backend (`base_dir` + `command_dir`). Registered as `"local"` in its own `init()` |
| `fs_mcp.go` | MCP SDK backend over HTTP SSE. `remote_root`, `url`, custom `headers`. Uses `exchange.FileExchange` for remote exec |
| `jumpserver.go` | JumpServer API backend via REST. Token auth with auto-login. No exec support |

### IMPLEMENTING A BACKEND

1. Implement `FileTransferBackend` — `ListDir`, `Read`, `Write`, `Delete`, `SupportsExec`, `Exec`, `Ping`
2. Return `ErrNotSupported` from `Exec` if unsupported; set `SupportsExec()` = `false`
3. Write constructor: `func NewXxxBackend(config map[string]interface{}) (FileTransferBackend, error)`
4. Register: `func init() { RegisterBackend("name", NewXxxBackend) }`
5. Import the package in `cmd/relay/main.go` (side-effect import)

### CONVENTIONS

- Constructors type-assert each key from `map[string]interface{}`
- `init()` auto-registration — no manual wiring
- `ErrNotSupported` for optional methods
- Each backend has its own `resolvePath()` helper

### ANTI-PATTERNS

- Do NOT register backends in `backend.go`'s `init()`. Each backend owns its own `init()` + file
- Do NOT use a switch/enum for backend selection — the registry replaces dispatch

## COMMANDS

```bash
make build          # Build binary (./relay)
make test           # go test -v -race ./...
make fmt            # go fmt ./...
make vet            # go vet ./...
make build-release  # Stripped, CGO_ENABLED=0
```

## internal/watcher/

Daemon polling remote directories and executing job chains on file pattern matches.

### WHERE TO LOOK
| Symbol | File:Line | Role |
|--------|-----------|------|
| `Watcher` struct | `watcher.go:24` | Core state: cfg, configPath, processed, jobResults, pendingConfig |
| `Run()` | `watcher.go:47` | Main ticker loop, calls `runOnce()` each interval |
| `processWatch()` | `watcher.go:429` | Lists remote dir, glob-matches files, triggers `executeJobs()` |
| `executeJobs()` | `watcher.go:476` | Sequential job execution with `if:` condition gating |
| `processCommandsLoop()` | `watcher.go:79` | Background goroutine: polls command dir for sync/exec cmds |
| `handleConfigSync()` | `watcher.go:300` | Decodes base64 payload, validates, backs up, stages for next cycle |
| `applyPendingConfig()` | `watcher.go:363` | Atomic rename of staged config at top of `runOnce()` |

### KEY BEHAVIOR
- **Two concurrent loops**: main ticker (watch processing) + command processor (config sync, remote exec)
- **Staged config reload**: sync decoded/validated in command loop, staged via `pendingConfig`, applied via atomic rename at start of next `runOnce()`
- **processed map**: dual-purpose (file paths + cmd paths); capped at 5000 entries via `cleanupProcessedMap()`; reset is lossy, seen files may re-trigger
- **Job conditions**: `jobs.<id>.success` / `jobs.<id>.failure` evaluated via `evaluateCondition()`
- **Variable expansion**: `{file_path}`, `{file_name}`, `{file_dir}`, `{file_remote_path}`, `{timestamp}`
- **Heartbeat**: goroutine writes timestamp to `commandDir/.heartbeat` every 5s
- **Adaptive backoff**: command polling starts at 2s, doubles to 30s max when idle

### CONVENTIONS
- Watch dirs processed in parallel via `errgroup.Group`
- Backend created per-watch via `NewBackend()` factory (not hardcoded)
- `exec.CommandContext` + `context.WithTimeout` for job timeouts; no sleeping during jobs

### ANTI-PATTERNS
- `processed` map reset clears all tracked state, can cause re-triggers on active dirs
- `runLocalCommand()` logs to stdout/stderr but discards output from job results tracking
- Command processor uses same `processed` map as file watcher (shared namespace collision risk)

## NOTES

- `processed` map in watcher can grow unbounded (was fixed in 15ff849)
- Config hot-reload: staged at sync, applied on next cycle (not immediate)
- Polling-based architecture (no filesystem events)
- Module path is `github.com/user/relay` — update before publishing
