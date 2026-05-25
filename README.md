# file-exchange

Generic File Exchange Command Execution System

A Go-based bidirectional file exchange system where a single node can push files to or pull files from a remote server, with configurable trigger actions (shell scripts) executed on the receiving end.

## Features

- **Backend Abstraction**: Support for multiple backends (local filesystem, fs-mcp, JumpServer)
- **Watcher Mode**: Poll remote directories for files matching glob patterns
- **Action Types**: Execute shell commands or delete files
- **Variable Substitution**: Use built-in variables like `{file_path}`, `{file_name}`, `{file_dir}`, `{timestamp}`
- **Conditional Execution**: Control job execution with `if` conditions
- **File Exchange Protocol**: JSON-based command exchange via `cmd-{uuid}.json` / `result-{uuid}.json`

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

1. Copy the example configuration:

```bash
mkdir -p ~/.file-exchange
cp config.example.yaml ~/.file-exchange/config.yaml
```

2. Edit `~/.file-exchange/config.yaml` with your settings.

### Configuration Reference

```yaml
name: file-exchange
version: 1

backend:
  type: local  # local, fs-mcp, or jumpserver
  config:
    base_dir: /tmp/file-exchange
    command_dir: commands

auth:
  method: proxy  # required for fs-mcp/jumpserver
  login_url: https://example.com/login
  token_cookie_name: session_token
  proxy_port: 8080
  token_cache_file: ~/.file-exchange/token.json

watchers:
  - name: Apply Patch
    on:
      file_match: "*.patch"
      watch_dir: patches
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
        cwd: /path/to/repo
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success  # optional condition

interval_seconds: 60
```

## Usage

### Watch Mode (Daemon)

Watch mode is a persistent process that polls remote directories and executes actions.

```bash
file-exchange watch --config ~/.file-exchange/config.yaml
```

### Watch Mode (Single Iteration)

```bash
file-exchange watch --config ~/.file-exchange/config.yaml --once
```

### Push Mode (One-shot CLI)

Push mode is a one-shot CLI command that uploads files to the remote server and exits.

```bash
# Push a single file
file-exchange push <source> <dest>

# Push a directory (recursive)
file-exchange push /local/path/ /remote/path/
```

Examples:

```bash
# Push a patch file
file-exchange push ./my.patch patches/my.patch

# Push entire directory
file-exchange push ./build/ release/v1.0/

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

## Action Types

### exec

Execute a shell command via the file exchange protocol.

```yaml
- id: apply
  type: exec
  cmd: git am --3way {file_path}
  cwd: /path/to/repo
```

### file_delete

Delete a remote file.

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
| `{file_remote_path}` | Same as file_path (remote path) |
| `{timestamp}` | Current time in RFC3339 format |

## Conditional Execution

Use `if` conditions to control job execution based on previous job results:

```yaml
- id: cleanup
  type: file_delete
  path: "{file_remote_path}"
  if: jobs.apply.success  # run only if 'apply' succeeded
```

Supported conditions:
- `jobs.<id>.success` - previous job completed successfully
- `jobs.<id>.failure` - previous job failed

## Shell Responder

For air-gap machines, deploy the shell responder script:

```bash
# On the air-gap machine
./command-responder.sh
```

The responder polls the `command_dir` for `cmd-*.json` files, executes the command, and writes `result-*.json` files.

## Development

### Build

```bash
make build
```

### Test

```bash
make test
```

### Format

```bash
make fmt
```

## License

MIT