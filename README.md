# file-exchange

Generic File Exchange Command Execution System

A Go-based tool for monitoring remote directories and executing actions when files matching patterns are detected.

## Features

- **Watch Mode**: Monitor multiple remote directories, trigger actions on matching files
- **Multiple Paths**: One watch supports multiple file patterns
- **Multiple Jobs**: Each watch supports multiple jobs to execute
- **Push Mode**: Upload files to remote directories
- **Exec Mode**: Execute commands locally

## Installation

```bash
make build
# or
go install ./cmd/file-exchange
```

## Configuration

```yaml
name: file-exchange
version: 1

backend:
  type: local
  config:
    base_dir: /path/to/remote/storage

# Watch configurations
watch:
  - watch_dir: projects/app/patches
    paths: ["*.patch"]
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success

  - watch_dir: projects/app/deploy
    paths: ["deploy-*.yaml", "deploy-*.yml"]
    jobs:
      - id: deploy
        type: exec
        cmd: ./deploy.sh {file_name}

interval_seconds: 60
```

## Usage

### Watch - Daemon Mode

```bash
# Start watcher daemon
file-exchange watch

# Run single iteration
file-exchange watch --once
```

### Push - Upload Files

```bash
file-exchange push -d projects/app/patches ./my.patch
```

### Exec - Local Commands

```bash
file-exchange exec "npm run build"
```

## Config Structure

| Field | Description |
|-------|-------------|
| `watch_dir` | Remote directory to monitor |
| `paths` | Array of glob patterns to match files |
| `jobs` | Actions to execute when file matches |

## Job Types

### exec

Execute a shell command when file is detected.

```yaml
- id: apply
  type: exec
  cmd: git am --3way {file_path}
  cwd: /path/to/repo  # optional working directory
```

### file_delete

Delete the detected file.

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
- `jobs.<id>.success` - job completed successfully
- `jobs.<id>.failure` - job failed

## Development

```bash
make build   # Build binary
make test    # Run tests
make fmt     # Format code
```

## License

MIT