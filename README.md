# file-exchange

Generic File Exchange Command Execution System

A Go-based bidirectional file exchange system with device management. Push files or execute commands on remote devices through a simple CLI, while a watcher daemon can process files automatically.

## Features

- **Device Management**: Configure multiple remote devices with path mappings
- **Push Mode**: One-shot CLI to upload files to remote devices
- **Exec Mode**: Execute commands on remote devices via file exchange protocol
- **Watch Mode**: Daemon process to monitor and process files automatically
- **Backend Abstraction**: Support for local, fs-mcp, and JumpServer backends
- **Variable Substitution**: Use built-in variables like `{file_path}`, `{file_name}`
- **Conditional Execution**: Control job execution with `if` conditions

## Installation

### From Source

```bash
git clone <repository>
cd file-exchange
make build
```

### Using Go Install

```bash
go install ./cmd/file-exchange
```

## Configuration

```bash
mkdir -p ~/.file-exchange
cp config.example.yaml ~/.file-exchange/config.yaml
```

### Configuration Reference

```yaml
name: file-exchange
version: 1

# Default backend (used if device not specified)
backend:
  type: local
  config:
    base_dir: /tmp/file-exchange
    command_dir: commands

# Device configurations
devices:
  - id: dev-server-01
    name: Development Server 1
    backend:
      type: local
      config:
        base_dir: /tmp/file-exchange
        command_dir: commands
    paths:
      watch_dir: patches      # where watcher monitors files
      remote_dir: /remote     # base remote directory
      command_dir: commands   # where cmd-{uuid}.json files go

# Watcher configurations (by device)
watchers:
  - name: Apply Patch
    device: dev-server-01     # which device to watch
    on:
      file_match: "*.patch"
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
        cwd: /repo
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success

interval_seconds: 60
```

## Usage

### Push - Upload Files

```bash
# Push a file to device's watch directory
file-exchange push -d <device_id> <source_file>

# Examples:
file-exchange push -d dev-server-01 ./my.patch
file-exchange push -d prod-server-01 ./build.tar.gz
```

### Exec - Run Remote Commands

```bash
# Execute command on remote device
file-exchange exec -d <device_id> "<command>"
file-exchange exec -d <device_id> --cwd /path/to/dir "<command>"

# Examples:
file-exchange exec -d dev-server-01 "ls -la"
file-exchange exec -d prod-server-01 --cwd /app "docker-compose up"
```

### Watch - Daemon Mode

```bash
# Start watcher daemon
file-exchange watch

# Run single iteration
file-exchange watch --once
```

## Backends

### Local

Direct filesystem access for testing or local deployments.

```yaml
backend:
  type: local
  config:
    base_dir: /tmp/file-exchange
    command_dir: commands
```

### fs-mcp

Model Context Protocol filesystem server for remote access via HTTP.

```yaml
backend:
  type: fs-mcp
  config:
    url: http://localhost:3000/mcp
    command_dir: /commands
    headers:
      Cookie: "session=xxx"
```

### JumpServer

JumpServer REST API integration for enterprise environments.

```yaml
backend:
  type: jumpserver
  config:
    base_url: https://jumpserver.example.com
    username: admin
    password: secret
    command_dir: /commands
```

**Note**: JumpServer backend does not support `exec` action type (file operations only).

## File Exchange Protocol

For `exec` action and `exec` command, the system uses a file-based protocol:

1. Write `cmd-{uuid}.json` to command directory
2. Remote responder executes the command
3. Responder writes `result-{uuid}.json` with output
4. Original node reads result and deletes both files

## Action Types (Watch Mode)

### exec

```yaml
- id: apply
  type: exec
  cmd: git am --3way {file_path}
  cwd: /path/to/repo
```

### file_delete

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
  if: jobs.apply.success  # run only if 'apply' succeeded
```

## Development

```bash
make build      # Build binary
make test       # Run tests
make fmt        # Format code
```

## License

MIT