# relay

Generic File Exchange Command Execution System

A Go-based daemon for monitoring remote directories and executing actions when files matching patterns are detected. Supports pluggable backends (native relay WebSocket, local, MCP, JumpServer) and config hot-reload without restart.

## Features

- **Watch Mode**: Monitor remote directories, trigger exec/file_delete jobs on file pattern matches
- **Event-Driven Mode**: Real-time file change events via WebSocket (relay backend) — no polling
- **Multiple Paths**: One watch supports multiple file glob patterns
- **Multiple Jobs**: Each watch supports a chain of jobs with conditional execution (`if: jobs.X.success/failure`)
- **Push/Pull**: Upload and download files with streaming transfer and zstd compression
- **Remote Exec**: Forward commands to remote server via WebSocket
- **Config Hot-Reload**: Push config to a running watcher without restart
- **Pluggable Backends**: Local filesystem, native relay (WebSocket), MCP SDK (HTTP SSE), JumpServer API
- **Backend Auto-Discovery**: New backends register via `init()` — no switch/enum dispatch
- **Connection Resilience**: Automatic reconnection with exponential backoff, heartbeat health checks

## Installation

```bash
make build
# or
go install ./cmd/relay
```

## Backends

| Backend | Type | Exec | Events | Description |
|---------|------|------|--------|-------------|
| `relay` | plugin | yes | yes | Native WebSocket protocol — streaming, zstd compression, real-time events |
| `local` | built-in | yes | no | Local filesystem with `base_dir` and `command_dir` |
| `fs-mcp` | plugin | yes | no | MCP SDK backend over HTTP SSE |
| `jumpserver` | plugin | no | no | JumpServer REST API with token auth |

When using the `relay` backend, the watcher automatically switches from polling to **event-driven mode** — file changes are pushed in real-time via WebSocket instead of periodic directory listing.

## Configuration

```yaml
name: relay
version: 1

backend:
  type: local
  config:
    base_dir: /path/to/remote/storage

watch:
  - id: web-app-patches
    watch_dir: projects/web-app/patches
    local_dir: /path/to/local/repo
    paths: ["*.patch"]
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success

  - id: web-app-deploy
    watch_dir: projects/web-app/deploy
    local_dir: /path/to/local/repo
    paths: ["deploy-*.yaml"]
    jobs:
      - id: deploy
        type: exec
        cmd: ./deploy.sh {file_name}

interval_seconds: 60
```

**Windows note:** When running on Windows (MSYS/Git Bash), `local_dir` can be written as `/d/...` and will be normalized to `D:\...` automatically.

## Usage

All commands load config via `-c` / `--config` (default: `~/.relay/config.yaml`). Enable debug output with `--debug`.

```bash
# Global flags
relay -c ~/.relay/config.yaml --debug <command>
```

Note: on `pull`, the short flag `-d` means `--delete` (delete the remote file after pulling), not debug. Debug is long-form `--debug` only.

The `push`, `pull`, `list`, `exec` and `job run` commands take an optional `-w`
/ `--watch` workspace. When omitted, the current directory's name is matched
against the configured workspace IDs. If it matches exactly one workspace that
one is used; if it matches none (or several) an error lists the available
workspaces instead.

### watch — Daemon Mode

```bash
relay watch -c ~/.relay/config.yaml
```

### push — Upload Files

```bash
relay push -w web-app-patches ./my.patch
```

Pushes file or directory to the watch's `watch_dir` on the remote backend.

### pull — Download File

```bash
relay pull -w web-app-patches remote-filename.patch
relay pull -d remote-filename.patch   # infer workspace from cwd, delete remote after
```

Downloads a file from the watch's `watch_dir` to the current directory. With
`--delete` / `-d`, the remote file is removed after a successful local write;
a failed delete is reported but does not fail the pull.

### list — List Remote Directory

```bash
relay list -w web-app-patches
```

Lists files in the watch's remote `watch_dir` with size and modification time.

### exec — Forward Command to Remote

```bash
relay exec -w web-app-patches "npm run build"
```

Forwards a command to the remote backend for execution. Requires a backend that supports `Exec` (local, fs_mcp). The `-w` flag sets the working directory to the watch's `local_dir`. When omitted, `-w` is inferred from the cwd when it can be; otherwise `exec` falls back to its no-workspace behavior.

### job run — Run a Config Job Locally

```bash
relay job run apply ./my.patch            # infer workspace from cwd
relay job run -w web-app-patches apply ../my.patch
```

Manually runs a job defined under the workspace's `jobs` in config, on the local machine. The optional file argument is bound to the `{file_path}`, `{file_name}` and `{file_dir}` variables. `exec` jobs run locally (defaulting to the workspace's `local_dir`), `file_delete` jobs delete the given local file. Job `if` conditions are ignored for manual runs. A job that references a file variable but is invoked without a file — or that fails — exits non-zero.

### cleanup — Remove Stale Command Files

```bash
relay cleanup -w web-app-patches
```

Removes stale `cmd-*.json` and `result-*.json` files from the remote `command_dir`.

### sync — Config Hot Reload

Push a new config to a running watcher without restarting it.

```bash
relay sync -c ~/.relay/config.yaml
```

**How it works:**
1. CLI reads the local config file and writes it as a command file in the shared `command_dir`
2. Watcher picks up the command, validates the config, creates a backup at `<configPath>.bak`, and stages it
3. New config applies on the next watch cycle (not immediately)

**Requirements:**
- Watcher must be running and started with `-c` flag
- `command_dir` must be consistent between watcher and CLI (default: `/tmp/relay-commands`)
- On validation failure, current config is preserved (no changes made)

### server — Relay Server

Run the relay server that serves file storage and event broadcasting.

```bash
# With YAML config file
relay server --server-config server.yaml

# With CLI flags only
relay server --addr :8443 --watch web-app:/data/web-app --token secret

# CLI flags override config file values
relay server --server-config server.yaml --addr :9000
```

**Server config** (`server.yaml`):

```yaml
addr: ":8443"

watch:
  - id: web-app-patches
    dir: /data/relay/web-app/patches
  - id: api-service-patches
    dir: /data/relay/api-service/patches

auth:
  tokens:
    - "${RELAY_TOKEN}"

tls:
  enabled: false
  cert_file: ""
  key_file: ""
```

Environment variables (`$VAR`, `${VAR}`) are expanded in all string values.

### ws — List Workspaces

```bash
relay ws              # list workspace IDs
relay ws -v           # verbose table with remote/local dirs and job counts
relay ws --json       # output as JSON
relay ws -w web-app   # details for a specific workspace
```

## Relay Backend Configuration

When using the `relay` backend, configure the WebSocket connection to the relay server:

```yaml
backend:
  type: relay
  config:
    url: "wss://server:8443/relay"     # WebSocket URL
    token: "${RELAY_TOKEN}"            # authentication token
    watch_id: web-app-patches          # watch ID to subscribe to on server
    watch_dir: .                       # remote directory (relative to server's watch dir)
    command_dir: /tmp/relay-commands   # command directory for config hot-reload
```

| Field | Description |
|-------|-------------|
| `url` | WebSocket URL (`ws://` or `wss://`) |
| `token` | Auth token (must match server config) |
| `watch_id` | Server-side watch ID to subscribe to |
| `watch_dir` | Remote directory path (relative to server's watch dir) |
| `command_dir` | Shared directory for config sync commands |

**Transfer features** (automatic, no config needed):
- 64KB chunked streaming for large files
- zstd compression on all chunks
- WebSocket binary frames (no base64 overhead)
- SHA256 digest verification
- Exponential backoff reconnection (1s→30s, 10 retries)
- 30s heartbeat with 90s timeout

## Example Configs

| File | Description |
|------|-------------|
| `config.example.relay.yaml` | Client config with native relay backend |
| `config.example.fs-mcp.relay.yaml` | Client config with MCP backend |
| `config.example.jumpserver.relay.yaml` | Client config with JumpServer backend |
| `server.example.yaml` | Server configuration |

## Config Structure

| Field | Description |
|-------|-------------|
| `name` | Config name |
| `version` | Schema version (currently 1) |
| `backend` | Backend type + config |
| `watch` | List of watch configurations |
| `watch[].id` | Unique watch identifier |
| `watch[].watch_dir` | Remote directory to monitor |
| `watch[].local_dir` | Local working directory for job execution |
| `watch[].paths` | Array of glob patterns to match files |
| `watch[].jobs` | Actions to execute when file matches |
| `watch[].auto_cleanup` | Delete remote file after all jobs succeed (default: false) |
| `interval_seconds` | Watch poll interval (default: 60, ignored in event-driven mode) |

## File Cleanup

Two complementary strategies to prevent files from accumulating on the server:

### Watcher-side: `auto_cleanup`

When `auto_cleanup: true`, the watcher deletes the remote file after all jobs succeed. Jobs that fail leave the file untouched for retry.

```yaml
watch:
  - id: web-app-patches
    auto_cleanup: true
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
```

### Server-side: `ttl`

When `ttl` is set on a watch directory, the server automatically deletes files older than the specified duration. Runs every minute.

```yaml
# server.yaml
watch:
  - id: web-app-patches
    dir: /data/relay/web-app/patches
    ttl: 30m     # delete files older than 30 minutes
```

Supported duration formats: `30s`, `5m`, `1h`, `24h`.

**Recommended**: Use both — `auto_cleanup` for immediate cleanup on success, `ttl` as a safety net for orphaned files.

## Job Types

### exec

Execute a shell command when file is detected.

```yaml
- id: apply
  type: exec
  cmd: git am --3way {file_path}
  cwd: /path/to/repo
  timeout: 120  # optional, seconds
```

### file_delete

Delete the detected file (or custom path).

```yaml
- id: cleanup
  type: file_delete
  path: "{file_remote_path}"
```

## Built-in Variables

| Variable | Description |
|----------|-------------|
| `{file_path}` | Full path to the watched file |
| `{file_name}` | Filename without directory |
| `{file_dir}` | Directory containing the file |
| `{file_remote_path}` | Remote path |
| `{timestamp}` | Current time in RFC3339 format |

## Conditional Execution

```yaml
- id: cleanup
  type: file_delete
  path: "{file_remote_path}"
  if: jobs.apply.success  # only execute if 'apply' succeeded
```

Supported conditions:
- `jobs.<id>.success` — job completed successfully
- `jobs.<id>.failure` — job failed

## Development

```bash
make build          # Build binary (./relay)
make test           # Run tests with race detector
make test-coverage  # Generate HTML coverage report
make fmt            # Format code
make vet            # Run go vet
make deps           # Download and tidy dependencies
make clean          # Remove build artifacts
make install        # Install binary to GOPATH
```

## License

MIT
