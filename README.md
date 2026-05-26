# relay

Generic File Exchange Command Execution System

A Go-based daemon for monitoring remote directories and executing actions when files matching patterns are detected. Supports pluggable backends (local, MCP, JumpServer) and config hot-reload without restart.

## Features

- **Watch Mode**: Monitor multiple remote directories, trigger exec/file_delete jobs on file pattern matches
- **Multiple Paths**: One watch supports multiple file glob patterns
- **Multiple Jobs**: Each watch supports a chain of jobs with conditional execution (`if: jobs.X.success/failure`)
- **Push Mode**: Upload files/directories to remote storage
- **Pull Mode**: Download single files from remote storage
- **List Mode**: List remote directory contents
- **Exec Mode**: Forward commands to remote backend
- **Config Hot-Reload**: Push config to a running watcher without restart
- **Pluggable Backends**: Local filesystem, MCP SDK (HTTP SSE), JumpServer API
- **Backend Auto-Discovery**: New backends register via `init()` — no switch/enum dispatch

## Installation

```bash
make build
# or
go install ./cmd/relay
```

## Backends

| Backend | Type | Exec Support | Description |
|---------|------|-------------|-------------|
| `local` | built-in | yes | Local filesystem with `base_dir` and `command_dir` |
| `fs_mcp` | plugin | yes | MCP SDK backend over HTTP SSE with `remote_root`, `url`, custom headers |
| `jumpserver` | plugin | no | JumpServer REST API with token auth and auto-login |

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

All commands load config via `-c` / `--config` (default: `~/.relay/config.yaml`). Enable debug output with `-d` / `--debug`.

```bash
# Global flags
relay -c ~/.relay/config.yaml -d <command>
```

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
```

Downloads a file from the watch's `watch_dir` to the current directory.

### list — List Remote Directory

```bash
relay list -w web-app-patches
```

Lists files in the watch's remote `watch_dir` with size and modification time.

### exec — Forward Command to Remote

```bash
relay exec -w web-app-patches "npm run build"
```

Forwards a command to the remote backend for execution. Requires a backend that supports `Exec` (local, fs_mcp). The `-w` flag sets the working directory to the watch's `local_dir`.

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
| `interval_seconds` | Watch poll interval (default: 60) |

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
