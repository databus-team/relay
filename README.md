# file-exchange

Generic File Exchange Command Execution System

A Go-based tool for managing multiple projects on a remote device. Push files to remote watch directories, execute local commands, and watch remote directories for automated processing.

## Features

- **Project-based**: Multiple projects with isolated watch directories on one device
- **Push Mode**: Upload files to remote project's watch directory
- **Exec Mode**: Execute commands locally (in current working directory)
- **Watch Mode**: Daemon to monitor remote directories and trigger actions

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

projects:
  - id: web-app
    name: Web Application
    watch_dir: projects/web-app/patches
    file_match: "*.patch"
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success

  - id: api-service
    name: API Service
    watch_dir: projects/api-service/patches
    file_match: "*.patch"
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
      - id: cleanup
        type: file_delete
        path: "{file_remote_path}"
        if: jobs.apply.success

interval_seconds: 60
```

## Usage

### Push - Upload Files

```bash
# Push file to project's watch directory
file-exchange push -p <project_id> <source_file>

# Examples:
file-exchange push -p web-app ./my.patch
file-exchange push -p api-service ./build.tar.gz
```

### Exec - Local Commands

Commands execute locally in the current working directory.

```bash
file-exchange exec -p <project_id> "<command>"

# Examples:
file-exchange exec -p web-app "npm run build"
file-exchange exec -p api-service "make test"
```

### Watch - Daemon Mode

Watch all projects defined in config:

```bash
# Start watcher daemon
file-exchange watch

# Run single iteration
file-exchange watch --once
```

## Project Configuration

Each project includes:

| Field | Description |
|-------|-------------|
| `id` | Unique project identifier |
| `name` | Display name |
| `watch_dir` | Remote directory to monitor |
| `file_match` | Glob pattern for files to watch |
| `jobs` | Actions to execute when file detected |

## Job Types

### exec

Execute a shell command when file is detected.

```yaml
- id: apply
  type: exec
  cmd: git am --3way {file_path}
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
  if: jobs.apply.success
```

## Development

```bash
make build   # Build binary
make test    # Run tests
make fmt     # Format code
```

## License

MIT