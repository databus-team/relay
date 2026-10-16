# relay

Generic File Exchange Command Execution System

A Go-based daemon for monitoring remote directories and executing actions when files matching patterns are detected. Supports pluggable backends (native relay WebSocket, local, MCP, JumpServer) and config hot-reload without restart.

## Features

- **Watch Mode**: Monitor remote directories, trigger exec jobs on file pattern matches
- **Event-Driven Mode**: Real-time file change events via WebSocket (relay backend) — no polling
- **Multiple Paths**: One watch supports multiple file glob patterns
- **Multiple Jobs**: Each watch supports a chain of jobs with conditional execution (`if: jobs.X.success/failure`)
- **Push/Pull**: Upload and download files with streaming transfer and zstd compression
- **Remote Exec & Git Sync**: Forward commands via WebSocket with real-time streaming; incremental Git diff sync runs **by default** before execution (opt out with `--no-sync`), with shadow snapshot protection
- **Task Supervisor (Docker-style UX)**: Detached background execution (`exec -d`), task listing (`ps`), live log tailing (`logs -f`), and process group termination (`stop`)
- **Port Forwarding (Docker-style -p)**: Forward local ports to remote command/service (`relay run -p 8080:8080`) over full-duplex tunnel stream without requiring network allowlist rules
- **SOCKS5 Egress Tunnel (node-held)**: `relay run -t tunnel -p 127.0.0.1:1080 -e <egress>` opens a local SOCKS5 endpoint whose egress runs on a chosen executor's network (`network_allow` enforced executor-side, deny by default); held by the local `relay node` like `docker run`, persisted to `~/.relay/tunnels.json`, replayed on node restart (`--restart=always|no`), and operated by `relay ps`/`start`/`restart`/`stop`/`logs`
- **Unified Object Lifecycle**: tasks and tunnels share one verb set — `relay run` creates, `relay start`/`restart`/`stop`/`logs` act on existing objects by name (task ID or tunnel egress)
- **One-Click Undo & Drift Diff**: Restore overwritten remote workspaces instantly (`relay undo`) and inspect cross-node drift & file conflicts (`relay diff`)
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

workspaces:
  - id: web-app-patches
    watch_dir: projects/web-app/patches
    local_dir: /path/to/local/repo
    paths: ["*.patch"]
    auto_cleanup: true        # jobs 全部成功后自动删除中转 watch_dir 源文件
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
      - id: cleanup_local
        type: exec
        cmd: rm -f {file_path}   # 删除类工作统一用 exec

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

In the example above, whenever a file matching `*.patch` appears in
`watch_dir`, the watcher syncs it into `local_dir` and runs the `exec` jobs
**locally** (the backend only transfers files). `{file_path}` therefore points
to the local copy; `{file_remote_path}` refers to the remote file for cleanup.

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

### node — Peer Node Daemon (hub + agent)

One process holds both roles, selected by `node.roles` in the config: `hub` serves the transit
(reading the `server:` block), `agent` self-registers as an executor and runs pushes/execs/tunnel
egress. This one command replaces the retired `relay server` / `relay watch` daemons.

```bash
relay node run   -c ~/.relay/config.yaml   # foreground
relay node start -c ~/.relay/config.yaml   # daemon; also: stop | status | restart | upgrade
```

### push — Upload Files

```bash
relay push -w web-app-patches ./my.patch
```

Pushes file or directory to the watch's `watch_dir` on the remote backend.

With the **relay** backend, a single-file push is **transparent**: the file is delivered directly to the online remote executor's project
directory (`executor_dir`), and the executor runs that file's **jobs automatically**, streaming job output back to the local command.
Arbitrarily large files (a multi-MB `relay` binary, a big patch) are handled: the content is streamed hop-by-hop in 64KB zstd-compressed
binary chunks instead of being sent as one giant base64 message. If no executor is online, `push` falls back to staging the file on the
transit server's `watch_dir` (the classic watch-and-pull flow) and prints a `[staged ...]` notice. Directories still use the plain staging path.

- `--no-jobs`: transfer only — deliver the file without triggering the workspace's jobs on the remote executor.
- `--dest <path>`: override the target destination with an absolute path, bypassing `watch_dir`. Requires `--no-jobs`.
- `-e/--executor <id>`: address a specific executor directly by its `executor_id` (no workspace binding needed) — mutually exclusive with `-w`. Useful with `--no-jobs`/`--dest` for node-level ops (e.g. binary deploys to an executor that has no bound workspace).

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

### exec / run — Forward Command to Remote & Background Task

`relay run` is an alias for `relay exec`. Commands support both `--node` and `--executor` (or `-e`):

```bash
# Direct execution (interactive streaming)
relay run -w web-app-patches "npm run build"
relay run --node linux-heavy "go test -v ./..."

# Incremental Git sync of local workspace changes runs by default
relay run -w web-app-patches "pytest tests/"            # syncs the local diff, then runs
relay run -w web-app-patches --no-sync "pytest tests/"  # skip sync

# Forward local port 8080 to remote 8080 while running command (Docker -p paradigm)
relay run -w web-app-patches -p 8080:8080 "npm run dev"

# Detached execution: Run in background (returns Task ID immediately, Docker-like UX)
relay run -w web-app-patches -d "npm run dev"
# Output: t-a1b2c3d4-1718000000

# Detach with a restart policy: the executor re-runs it on its next start
relay run -w web-app-patches -d --restart=always "npm run dev"
relay run -w web-app-patches -d --no-restart "npm run dev"   # shortcut for --restart=no (the default)

# Tunnel mode: create a node-held SOCKS5 tunnel instead of running a command
relay run -t tunnel -p 127.0.0.1:1080 -e site-a
relay run --type=tunnel -p 0.0.0.0:1080 -e site-a --restart=always
```

Forwards a command to the remote side for execution. Requires a backend that supports `Exec` (local, fs_mcp, relay). The `-w` flag sets the working directory to the watch's `local_dir`. When omitted, `-w` is inferred from the cwd when it can be; otherwise `run/exec` falls back to its no-workspace behavior. Use `--node / -e <id>` to address a specific node by its ID directly.
- **`-p, --publish [host:][local:]remote`**: Forward local ports to the remote command (e.g. `-p 8080`, `-p 3000:8080`, or `-p 0.0.0.0:8080` to bind a non-loopback host), automatically tearing down the forwarders when the command exits. Temporary development port forwards do not require `network_allow` whitelist rules.
- **`-t, --type task|tunnel|forward`**: what `relay run` creates. Default `task` runs a command on the remote; `tunnel` creates a node-held SOCKS5 tunnel *instead*; `forward` creates a **node-held port-forward-only** *instead*. Both non-task modes take exactly one `-p` (`tunnel`: `[host:]port`, no remote half; `forward`: `[bind:]local[:remote_host:remote_port]`), pick the egress via `-e/--executor`, and take no positional command. Task-only flags (`-d`, `--no-sync`, `-f`, `-w`) are rejected in both non-task modes rather than ignored. See the [tunnel section](#tunnel--local-socks5-egress-tunnel) and [port-forward-only](#port-forward-only).
- **`--restart always|no` / `--no-restart`**: the restart policy. For tasks it requires `-d` (a foreground run has no managed task to carry it) and is written to the task metadata; in tunnel/forward mode it is written to the intent. In all cases the only recovery event is the next start of the owning daemon (executor for tasks, local node for tunnels/forwards).
- **Detach + publish (`` `relay run -d -p `...` ``)** keeps the local ports alive after the CLI exits via the local `relay node` daemon. The node runs as `agent` (omitting `node.roles` defaults to `agent`; an explicit `roles: [hub]` is rejected with a clear error); `relay run -d -p` auto-starts it if needed. The local control socket `~/.relay/forward.sock` accepts register/unregister from any local process (unauthenticated) — keep the machine trusted; it is not exposed over the network.
- **Git sync (ON by default — opt out with `--no-sync`):** Automatically detects uncommitted local Git modifications and untracked files, packs an incremental diff payload, and applies it to the remote workspace before executing the command. Includes base commit drift validation and automatic remote shadow snapshot backup (`refs/relay/backups/*`). Note: it carries **only the local working-tree diff, never unpushed commits**, and its source is the workspace's `local_dir` — which must be a valid Git repo **on the CLI machine** (otherwise sync is silently skipped).
- `-f, --force-remote`: Force a sync even when the local tree is clean (creates a shadow snapshot on the remote first, then overwrites remote state).
- `-d, --detach`: Runs the command in the background under the node's **TaskSupervisor**. Process trees are tracked cleanly (no orphaned child processes), stdout/stderr are persisted to `~/.relay/tasks/<task-id>/run.log`, and real-time output is broadcasted to subscribers.
- **Foreground cap: 30s.** A foreground `run`/`exec` is capped at 30 seconds on the executor side and there is no CLI flag to raise it — long-running commands or servers must use `-d` (detached), then `relay logs <task-id> -f`.

### ps — List Managed Background Tasks

```bash
# List active running tasks on an executor node
relay ps -e node-1

# List all tasks (including stopped / exited)
relay ps -e node-1 -a
```

Displays tasks managed by the node's TaskSupervisor across macOS, Linux, and Windows:
```
TYPE   TASK ID               NODE   STATUS  PORTS          RESTART EXIT CREATED COMMAND
task   t-a1b2c3d4-1718000000 node-1 running 8080           always  -    12m ago npm run dev
task   t-e5f6g7h8-1718000500 node-1 exited  -              no      0    3h ago  pytest tests/ (exit: 0)
tunnel site-a                site-a running 127.0.0.1:1080 always  -    3m ago  egress site-a
```

Column widths come from the content, and the table adapts to the terminal: when the output is wider than
the window it is narrowed (never wrapped) and over-long cells end in `...`. `--json` is lossless.

Rows come in three kinds, distinguished by the `TYPE` column: `task` rows are remote detach tasks,
`tunnel` rows are node-held SOCKS5 tunnels (egress in `COMMAND`, listen address in `PORTS`), and
`forward` rows are node-held port-forwards (identity is the local port, executor in `NODE`).

- **`RESTART`** is the object's restart policy (`always`/`no`) — for a task the value recorded in its
  metadata by `relay run -d --restart=...`, for a tunnel the value in its intent.
- **`CREATED`** is the time the object was **first** created — for a `task` row. A restart (`relay restart`)
  only updates the last-start time, so a task's `CREATED` does not reset. A `tunnel` row instead shows its
  last (re)bind time, so `relay restart <egress>` does move it.
- **`PORTS`** is the *recorded intent*, not proof that the local port is being listened on right now (see
  the tunnel section).

### logs — Stream & Tail Task Logs

```bash
# Live stream task logs (similar to docker logs -f)
relay logs -e node-1 t-a1b2c3d4-1718000000 -f

# View recent lines without following
relay logs -e node-1 t-a1b2c3d4-1718000000 --tail 50
```

Subscribes to live task output broadcast or retrieves persisted log entries from disk.

### stop — Terminate Background Task or Tunnel

```bash
relay stop -e node-1 t-a1b2c3d4-1718000000   # task, by ID
relay stop site-a                            # tunnel, by egress
relay stop 7001                              # port-forward, by local port
```

Stops a running task. The executor cleans up the entire process tree using process groups/signals on Unix or `taskkill /T /F` on Windows, leaving no orphaned server processes behind.

`stop` also **clears the task's restart policy**, so a task carrying `--restart=always` does not come back
when the executor next starts. The record and its log are kept. On the tunnel side `stop` is `docker rm`
semantics: it closes the listener and **removes the intent**, so the tunnel is forgotten (see the
[tunnel section](#tunnel--local-socks5-egress-tunnel)). Stopping an already-stopped object is not an error.

### start / restart — Reopen an Existing Task or Tunnel

`relay run` creates objects; `start` and `restart` only act on objects that already exist. Both name the
object the same way — a **task ID**, a **tunnel egress**, or a **forward port** — and all are refused if the name is unknown
(with a pointer at how to create one). When a name matches a task and a tunnel at once, the task wins.

```bash
relay start t-a1b2c3d4-1718000000   # re-run a stopped/exited task under the same ID
relay start site-a                  # reopen a stopped node-held tunnel on its recorded listen address
relay start 7001                    # reopen a stopped node-held port-forward on its recorded port
relay restart t-a1b2c3d4-1718000000 # same, but stop the live instance first
relay restart site-a                # same, for a tunnel
relay restart 7001                  # same, for a forward
```

- **`start` never creates.** If nothing has that name, it errors instead of building one — use `relay run`
  (or `relay run -t tunnel`) for that.
- **`start` on something already running does not restart it** and does not fail; it just returns. The same
  goes for `stop` on something already stopped.
- **Restarting keeps identity.** A task keeps the same ID, the same task directory and its original
  `CREATED` time; only the last-start time moves. Only the recorded command, working directory and ports are
  replayed — not the original environment, shell choice, or git diff sync.
- **A `stop`ped tunnel is forgotten**, so `start <egress>` cannot bring it back; only `relay run -t tunnel`
  can, and it must name the listen address again.
- **Local port-forward rules come back via `start`/`restart`.** The rules live in the local node's memory
  and are dropped when the node restarts (and removed by `relay stop`), so a task that returns — whether by
  `relay start` or by the executor replaying it — does not bring its published local port back on its own.
  `relay start <task-id>` / `relay restart <task-id>` re-register them from the task's recorded ports
  (bind host included), idempotently.

Task-side verbs fail with a non-zero exit when the target executor is offline or the transit is
unreachable; only when that executor is known, or when you named it with `-e/--executor`, does the error
carry its executor ID. Tunnel-side verbs degrade when the local node is not running and say so, rather than
reporting "no such task or tunnel".

### nodes — List Cluster Topology & Roles

```bash
# List all peer nodes across topology (similar to kubectl get nodes)
relay nodes

# Output in JSON format
relay nodes --json

# Include live CPU / memory usage (heavier probe: one sampling window per node)
relay nodes --usage
```

Displays cluster topology with node identities, roles (`hub`, `agent`), operational readiness, platform architecture, and end-to-end latency:
```
NAME                 ROLES STATUS PLATFORM      CPU      MEMORY      VERSION        LATENCY
CT-MBP.local (local) agent Ready  darwin/arm64  8c       16.0G       v0.4.5+13f288f -
hub (transit)        hub   Ready  linux/amd64   8c [pod] 16.0G [pod] v0.4.5+13f288f 9 ms
extranet             agent Ready  windows/amd64 4c       12.0G       v0.4.5+13f288f 12 ms
intranet             agent Ready  linux/amd64   4c       8.0G        v0.4.5+13f288f 13 ms
```

With `-u` / `--usage` the `CPU` and `MEMORY` columns switch to live usage, both expressed as a percentage of what that node is allowed to use:
```
NAME                 ROLES      STATUS PLATFORM      CPU           MEMORY                VERSION        LATENCY
CT-MBP.local (local) agent      Ready  darwin/arm64  8c (24%)      2.2G/16.0G (14%)      v0.4.5+13f288f -
hub (transit)        hub, agent Ready  linux/amd64   8c (0%) [pod] 0.3G/16.0G (2%) [pod] v0.4.5+13f288f 6 ms
extranet             agent      Ready  windows/amd64 4c (5%)       5.7G/12.0G (47%)      v0.4.5+13f288f 111 ms
```
`CPU` reads as `<quota> (<used>)`: the quota is the cgroup CPU limit inside a container, otherwise the machine's core count. `MEMORY` is `used/total (percent)`. The `[pod]` suffix marks values read from the container's own cgroup rather than the host's — including in the CPU column, which on a pod is sampled from `cpu.stat` so a busy host can't be mistaken for a busy pod. Nodes running a build older than this protocol change report no percentage and simply show their static quota until they are redeployed; a genuinely idle node shows `(0%)` instead of dropping the column.

Both tables adapt to the terminal width instead of wrapping (a half-screen pane is the common case for
`relay nodes -u`): columns are laid out from their content and narrowed when needed, and a cell that still
does not fit is elided with `...`. The usage columns degrade to just the percentage (`24%`) before any
ellipsis, because the percentage is what `-u` was asked for. `relay nodes --json` stays lossless.

Nodes that registered before but are currently disconnected show `STATUS Offline` and `LATENCY -`. The transit keeps a **known-executor ledger** so offline nodes stay visible (persisted to `server.watch_root/known_executors.json`; everything loads as offline until the node re-registers). To remove offline entries for good, prune them:

```bash
relay nodes --prune-offline   # remove all known-offline executors from the ledger, prints the count
```


### diff — Cross-Node Workspace Drift & Conflict Analysis

```bash
relay diff -e node-1 [-w web-app-patches]
```

Inspects and compares local and remote Git statuses before synchronizing:
- **Base Commit Check**: Verifies if local and remote share the same base commit SHA, warning if base commits have diverged.
- **Local vs Remote Changes**: Summarizes dirty and untracked files on both sides.
- **⚡ Conflict Warning**: Flags files that are simultaneously modified on both local and remote, warning of potential overwrites.

### undo — One-Click Recovery of Remote Workspace

```bash
# List all available shadow snapshots on the remote node
relay undo -e node-1 --list

# Restore remote workspace to the latest shadow snapshot
relay undo -e node-1

# Restore to a specific snapshot ref
relay undo -e node-1 --snapshot refs/relay/backups/1718000000
```

Whenever an exec with Git sync applies changes onto a dirty remote workspace, a shadow snapshot (`refs/relay/backups/<timestamp>`) is automatically created beforehand. If an accidental overwrite occurs or remote tests alter the state, `relay undo` cleanly resets the remote workspace back to its pre-sync state (including unstaged and untracked files). Pre-undo changes are preserved in a safety snapshot to prevent accidental data loss.

### job run — Run a Config Job Locally

```bash
relay job run apply ./my.patch            # infer workspace from cwd
relay job run -w web-app-patches apply ../my.patch
```

Manually runs a job defined under the workspace's `jobs` in config, on the local machine. The optional file argument is bound to the `{file_path}`, `{file_name}` and `{file_dir}` variables. `exec` jobs run locally (defaulting to the workspace's `local_dir`). Job `if` conditions are ignored for manual runs. A job that references a file variable but is invoked without a file — or that fails — exits non-zero.

### cleanup — Remove Stale Command Files

```bash
relay cleanup -w web-app-patches
```

Removes stale `cmd-*.json` and `result-*.json` files from the remote `command_dir`.

### sync — Config Hot Reload

Push the unified config to a running remote watcher (executor) without restarting it. Because all three endpoints share one `config.yaml`, syncing the file brings the executor's `backend`/`watch`/`jobs` up to date; the transit server re-reads the same file on restart. Under a **relay** backend, sync streams the config straight to the executor over the WebSocket (no transit file staging).

```bash
relay sync -c ~/.relay/config.yaml              # target = this config's backend.executor_id (single-root default)
relay sync -c ~/.relay/config.yaml -e site-b     # target a specific executor by its executor_id (multi-executor)
```

- `--executor/-e <executor_id>`: target a specific executor's node identity (e.g. push the shared config to `site-b` when several executors share one transit). Empty / omitted = fall back to the requester's own `backend.executor_id` (single-root), same as `exec`/`push`.
- The synced file is validated on the executor and written to its `config_path` (default `~/.relay/config.yaml`); the executor's `relay node` picks it up on the next cycle.

**How it works:**
1. CLI reads the local config file and writes it as a command file in the shared `command_dir`
2. Watcher picks up the command, validates the config, creates a backup at `<configPath>.bak`, and stages it
3. New config applies on the next watch cycle (not immediately)

**Requirements:**
- Watcher must be running and started with `-c` flag
- `command_dir` must be consistent between watcher and CLI (default: `/tmp/relay-commands`)
- On validation failure, current config is preserved (no changes made)

### hub — Transit Configuration

The transit (hub role of `relay node`) is configured entirely by the file; there are no per-flag
overrides. Run it with `relay node start` / `relay node run` on the transit machine.

**One config, three ends.** The unified `config.yaml` carries a `server:` block (addr / watch_root / auth.tokens / tls) plus the `workspaces` list (each entry is a job-config, able to bind to a specific executor via `executor:`). Each end keeps the whole file and reads only its relevant section. Note that the hub applies config to how it listens/stores — a change there takes effect on **restart**.

```yaml
server:
  addr: ":8443"
  watch_root: /data/relay          # each workspace.watch_dir is relative to this
  auth:
    tokens: ["${RELAY_TOKEN}"]
  tls:
    enabled: false
    cert_file: ""
    key_file: ""

workspaces:
  - id: web-app-patches
    watch_dir: web-app/patches
    ttl: 30m
    # executor: site-a            # 绑定到这台 executor(值=其 backend.config.executor_id);留空=单根
```

The legacy standalone `server.yaml` (top-level `addr`/`watch`/`auth`/`tls`) is **no longer supported** — put the `server:` block into the shared `config.yaml`. Environment variables (`$VAR`, `${VAR}`) are expanded in all string values.

### deploy — Push New Binaries to Executors & Transit

`scripts/relay-deploy.sh` (wired to `make deploy-remote` / `deploy-transit` / `deploy`) rebuilds and
pushes the relay binary to your deployment.

- **`make deploy-remote`** delivers the binary to **every executor that is online** (from
  `relay status`), probing each one's OS/arch, cross-compiling, and pushing via
  `relay push --no-jobs --dest`. Offline executors are skipped (they can't receive a push anyway).
  With `RESTART=1` it then detached-swaps and restarts each executor and verifies **each** one has
  switched to the new commit (per-`executor_id` check in `relay status --json`).
- Narrow the target set with `RELAY_EXECUTORS`, a comma/space-separated list of executor
  `executor_id`s (only ones that are online are deployed):
  ```bash
  RESTART=1 RELAY_EXECUTORS="site-a,site-b" make deploy-remote
  ```
- **`make deploy-transit`** / **`make deploy`** additionally deploys the transit via the
  `server-remote` self-upgrade channel (see below).

`make deploy-remote` addresses each executor directly by its `executor_id`
(`relay exec --executor <id>` / `relay push --no-jobs --executor <id>`) — node-identity routing, so no
`workspaces[].executor` binding is needed; executors with no bound workspace (e.g. extranet) are deployed too.

### server-remote — One-Click Transit Upgrade

Deploy a freshly built relay binary to the transit server over a **controlled self-upgrade**
channel (no SSH, no manual code-server upload). The client streams the binary to the transit,
which verifies the sha256 digest, self-checks that the binary can start, sends a success ACK,
backups the running binary to `.prev`, atomically swaps, and restarts — then the client
reconnects and polls the transit version ledger to confirm the transit now runs the new build.

```bash
# One-click: build a linux relay and deploy it to the transit server
make deploy-transit

# Or invoke the channel directly against a local binary
relay server-remote --binary relay-linux -c ~/.relay/config.yaml \
  --expect "<Version>+<Commit>"   # optional: exact build identity the transit must report after swap
```

After the swap the client reconnects and polls the transit version ledger until it matches the
deployed build. By default the poll compares against **this local binary's** `version.String()`
— which is wrong when the deployed `relay-linux` is a fresh/`-dirty` build different from the
installed `relay`. `make deploy-transit` always passes `--expect "<version>+<commit>"` stamped
into the binary it sends, so the ledger matches exactly; pass it manually when invoking the
channel directly against a binary whose build identity differs from the local relay.

**Trust & availability:**

- **Auth is a hard gate.** The channel is only available on an already-authenticated
  (handshake-token) session, AND the server must explicitly configure an `auth.tokens`.
  When no token is configured, the upgrade channel is **closed by default** — distinct from
  the "no token ⇒ allow all" posture of the file channel. Token holders can swap+restart the
  transit binary; this is a known, reviewed trust extension.
- **Self-check before swap.** Digest mismatch or a binary that cannot start (`version`
  subcommand fails) aborts without touching the running instance.
- **No auto-rollback.** The pre-swap binary is kept as `<binary>.prev` for manual recovery.

**Manual fallback (first-seed / SSH-unreachable):** to receive this message the transit must
already run a build that recognizes it, so the very first transit seed is manual — upload the
new `relay-linux` via code-server and run `relay node upgrade <path> -c $HOME/.relay/config.yaml`
(or `relay node restart` to just reboot). On a failed upgrade, restore via `.prev` through the
same manual path.

### ws — List Workspaces

```bash
relay ws              # list workspace IDs
relay ws -v           # verbose table with remote/local dirs and job counts
relay ws --json       # output as JSON
relay ws --name web-app   # details for a specific workspace
```

### status — Link Health & Version in One Shot

One-shot check: `本地→中转→远程执行方` 三段时延(local→transit, transit→executor, 两段之和)+ 各端点版本台账,吸收原 `ping` 与 `version -r` 的职责于单一命令。每段报告的都是该跳的往返(RTT)耗时,`total` 是前两段 RTT 之和。

```sh
relay status          # three-segment latency + version ledger
relay status --json   # machine-readable, same fields as text
```

Only the `relay` backend measures all three segments. Other backends (local/fs-mcp/jumpserver) give the single reachable hop and mark the rest `N/A (not supported on <type> backend)`, keeping the same columns / JSON shape. When an executor is offline the `transit→executor` segment shows `N/A (executor offline)` and the command still succeeds — "transit up, executor down".

Example for the `relay` backend with an online executor:

```
status (relay backend)
  local→transit  12 ms
  transit        v1.2.3 (linux/amd64)
  executors:
    web-app-patches  transit→executor 45 ms  total 57 ms  (v1.2.3)
```

### version — Local Build Info

Show the local binary's build info. Cross-machine version drift is now covered by `relay status`;
the remote ledger was removed from `version` (no more `-r`), and `--json` still works.

```sh
relay version        # local build info only
relay version --json # machine-readable
```

The version string is stamped at build time (`make build-release` / `build-linux` / `build-windows`) from `git describe`, so every binary built from the same source carries the same identifier. Run `make install` to rebuild with the stamp. Executors report their version when they register (they re-register on (re)connect), so an executor that has *not* yet been swapped still shows its old build.

### tunnel — Local SOCKS5 Egress Tunnel

Run a local SOCKS5 endpoint that reaches intranet targets through a chosen executor's network egress.
The real TCP connection happens **on the executor** — it validates its own `network_allow` whitelist and
connects to the target; the transit only forwards bytes (never lands, never parses, never executes).
Whitelist is enforced **executor-side** and **deny by default**; an empty/missing `network_allow` rejects
every tunnel connect.

A tunnel is a long-lived object like a detach task, so it uses the same lifecycle surface: **`relay run`
creates it**, and `relay start`/`restart`/`stop`/`logs` operate on it by name. There is no `relay tunnel`
sub-command.

Tunnels are **held by the local `relay node`** (agent role): the node plays the dockerd role and owns the
local listener, while the CLI only sends control messages over a unix socket (`~/.relay/tunnel.sock`). A
tunnel therefore survives the launching shell/SSH session, and its intent is persisted to
`~/.relay/tunnels.json` (`docker run` runtime state, not config) so the node can replay it on its next start.

```sh
relay run -t tunnel -p 127.0.0.1:1080 -e site-a   # create; ensures the local node is running
curl --socks5-hostname 127.0.0.1:1080 http://intra.a.internal/ping

relay ps                                         # list node-held tunnels (TYPE tunnel rows) + detach tasks
relay restart site-a                             # reopen the same egress without retyping -p/-e/--restart
relay stop site-a                                # close + forget the tunnel (docker rm semantics)
relay logs site-a -f                             # stream the tunnel's log lines
```

`-p` takes exactly one `[host:]port`: the local listen address, with no remote half (a tunnel has no target
port to forward to) and no default value — a tunnel that publishes nothing would be useless, so omitting
`-p` is an error. The host part is the bind address and defaults to the loopback: `-p 1080` is
`127.0.0.1:1080`, while `-p 0.0.0.0:1080` binds every interface. The egress is selected with
`-e/--executor`, by the executor's node identity **executor_id** (the `executors[].executor_id` shown by
`relay status --json`); it is passed straight through to the transit and routed by executor_id, with no
workspace mapping. `-w` is **not** accepted in tunnel mode — it means workspace, and tunnels are not
workspace-scoped.

The tunnel row's STATUS carries egress reachability, because a live local listener says nothing about the
far end of the three-hop chain (local → transit → egress): `running (11ms)` when the egress answers,
`running (ready)` when it answers without a measured latency, `running (offline)`, `running (notready)`,
or `running (unknown)` when the transit can't be queried. The parenthesised value is the whole point of
the column, so `egress` is not repeated in it.

**Restart policy (`--restart=always|no`, default `no`).** The recovery event is the node's next start
(tunnels are goroutines inside the node process). `always` reopens the listener when the node starts; `no`
keeps the entry but shows it `stopped` (visible under `relay ps -a`). `--no-restart` is a shortcut for
`--restart=no`. Because **restarting the node drops every tunnel** (they live in that one process, as do the
detached `-p` port-forwards), use `--restart=always` if you depend on a tunnel, or reopen it per-tunnel with
`relay restart <egress>`.

**Unified control plane.** Node-held tunnels appear alongside remote detach tasks:

```sh
relay ps                 # TYPE column: `tunnel` rows (egress in COMMAND, listen in PORTS) + `task` rows
relay ps -a              # also lists `stopped` tunnels
relay start site-a       # reopen a stopped tunnel (never creates one)
relay restart site-a     # stop the live listener, then reopen it on the recorded address
relay stop site-a        # close + forget the tunnel
relay logs site-a -f     # streams the tunnel's lines from ~/.relay/node.log (prefix `[tunnel site-a]`)
```

A tunnel row's STATUS also reports egress reachability (`running (11ms)` / `(ready)` / `(offline)` /
`(notready)` / `(unknown)`), from the same cluster ledger `relay nodes` uses — one extra transit query,
issued only when a tunnel is actually running, bounded by an 800ms per-egress probe budget and degrading
to `unknown` rather than failing. `--json` exposes it as `egress_status` / `egress_ready`.

`relay ps` still lists tunnels when the transit is unreachable — only the remote task list degrades.

The table adapts to the terminal width and never wraps: columns are laid out from their content, then
narrowed proportionally (a column never drops below its header), and anything that still does not fit is
elided with `...` — the last column being the one that gives up the most room, since it is a free-form
command line. A long port list therefore shows as `8080:9000,909...`, and a tunnel STATUS keeps the
parenthesised verdict (`(11ms)`) even when it can no longer afford the `running` prefix. On a narrow
terminal (a half-screen pane) long commands are elided rather than wrapped; `--json` is lossless either
way and always carries the complete command line and the full port list, so it is the surface to parse
when a command is too long to read in the table. Eliding only happens when a width is known: with stdout
redirected or piped there is no terminal to measure (unless `COLUMNS` is set) and nothing is omitted.

**Two behaviours that are easy to misread:**

- **`PORTS` is the recorded intent, not a live check.** It is what the tunnel was created with; it does not
  prove the local port is listening right now. After a node restart the node replays only the entries whose
  policy is `always`, and after a `stop` there is nothing left to replay at all.
- **A `stop`ped tunnel is forgotten.** `relay stop <egress>` removes the intent from `~/.relay/tunnels.json`,
  so `relay start <egress>` will *not* recreate it — it can only reopen an entry that still exists. To build
  it again you must re-run `relay run -t tunnel -p <[host:]port> -e <egress>` with the listen address.

Other notes:

- Default bind is loopback `127.0.0.1`; binding a non-loopback address prints a loud warning to the caller's
  stderr (no auth) — the tunnel has no authentication of its own.
- The `relay node` must be running to hold the listener; `relay run -t tunnel` and `relay start <egress>`
  start it if needed, while `restart`/`stop`/`logs` never start it implicitly and report the degraded state.
- Server must enable the tunnel channel (`tunnel_enabled: true` in the `server:` section); otherwise
  tunnel requests are rejected.
- Optionally bound the concurrent-tunnel cap with `server.max_tunnels`; default 256.

**Migrating from the retired `relay tunnel` namespace.** The whole `relay tunnel` command is gone; here is
the old → new mapping:

| Old | New |
|---|---|
| `relay tunnel start -w X --listen A --restart=always` | `relay run -t tunnel -p A -e X --restart=always` |
| `relay tunnel stop -w X` | `relay stop X` |
| `relay tunnel restart -w X` | `relay restart X` |
| `relay tunnel status` | `relay ps` (retired earlier) |

Two flag changes to watch for: the egress selector went from `-w/--executor` to **`-e/--executor`**, and
`-w` now means *workspace* (and is rejected in tunnel mode); and the listen address went from
`--listen <addr>` to **`-p <[host:]port>`** (with `--bind-host` as the default host for a bare port).

**Upgrading from the old per-egress daemon model.** Earlier releases ran one detached `tunnel-<executor>`
daemon per egress. That is retired. Stop any leftover daemon first — `relay stop <egress>`, or
`kill $(cat ~/.relay/tunnel-<egress>.pid)` — otherwise it still holds the local port and creating a tunnel
fails to bind (it detects the stale daemon and reports the exact cleanup command). The old
`relay tunnel run` (foreground), `relay tunnel status` (use `relay ps`) and `relay tunnel upgrade` (use
`relay node upgrade`) were removed earlier; tunnel logs go through `relay logs`.

**Dual-executor / concurrent tunnels (multi-executor):** one transit can host **two (or more)** remote
executors at once, each on its own watch/workspace. Create two tunnels, each binding its own local port and
selecting its own egress — access each executor's intranet targets independently without interference.
Closing one tunnel never affects the others; `relay status` / the version ledger list every online executor
per watch so you can see which egresses are available:

```sh
# executor M1 (watch site-a) and M2 (watch site-b) both online on the same transit
relay run -t tunnel -p 127.0.0.1:1080 -e site-a   # egress M1
relay run -t tunnel -p 127.0.0.1:1081 -e site-b   # egress M2, independent

curl --socks5-hostname 127.0.0.1:1080 http://a-intra.pvt/00
curl --socks5-hostname 127.0.0.1:1081 https://b-intra.pvt/api
```

**`network_allow` is per-executor.** Each executor's `backend.config` carries the allowlist for the
egress it may reach. In a multi-executor deployment put each executor's own whitelist in that executor's
config (so the tunnel through site-a is gated by site-a's allowlist, the one through site-b by site-b's).
Format: `host|IP|CIDR[@port]` — `@` port is optional (`443`, `80,443`, `8000-9000`); missing `@` = any port.
The transit and local CLI never need `network_allow`; only the executor enforces it.

```yaml
# executor site-a's backend.config
network_allow:
  - "10.0.0.0/8@80,443"
  - "api.internal.com@443"
  - "192.168.0.5@22"
```

```yaml
# executor site-b's backend.config — a different intranet, its own whitelist
network_allow:
  - "172.16.0.0/12"
```

### port-forward-only — Node-Held TCP Port Forward

Creates a TCP port forward whose listener is held by the **local `relay node`** daemon — no command, no
detach task, no keep-alive process. It is the intended way to publish a port to a remote executor without
the keep-alive hack below.

```bash
relay run -t forward -p 7001:7001 -e extranet --restart=always
# forward 7001 started: 127.0.0.1:7001 -> 127.0.0.1:7001 via extranet (restart=always)

relay ps          # TYPE=forward row, identity=port, executor in NODE, STATUS carries egress latency
relay logs 7001   # tail the forward's [forward 7001]-prefixed lines in node.log
relay stop 7001   # close the listener and forget the intent (docker rm semantics)
relay start 7001  # reopen on the recorded spec (may auto-start the local node)
```

`-p` takes the same [host:][local:]remote spec as `relay run -d -p`. Because the liveness is owned by the
local node (not a task), the forward survives as long as the node runs and is replayed on node restart
according to `--restart=always|no`; intent lives in `~/.relay/forwards.json` and control goes over
`~/.relay/forward.sock`. It is the clean replacement for:

```bash
# the keep-alive hack: a dummy task that pins the forward, dies when its timeout expires, and leaves a
# zombie listener (port accepts then the tunnel to the dead target fails) that `relay ps` won't show
relay run -d -p 7001:7001 -e extranet 'echo forward-alive && timeout 86400 tail -f /dev/null'
```

Data-plane behaviour is identical to `relay run -d -p` (port-forward mode bypasses `network_allow`); only
the lifecycle differs. `relay run -t forward` takes exactly one `-p` and rejects task-only flags `-d`,
`--no-sync`, `-f`, and `-w`.

## Relay Backend Configuration

When using the `relay` backend, configure the WebSocket connection to the transit hub:

```yaml
backend:
  type: relay
  config:
    url: "wss://server:8443/relay"     # WebSocket URL
    token: "${RELAY_TOKEN}"            # authentication token
    executor_id: web-app-patches       # 此执行方的节点身份(executor_id);被 relay exec/push/... 按此路由
    watch_dir: .                       # remote directory (relative to server's watch dir)
    command_dir: /tmp/relay-commands   # command directory for config hot-reload
```

| Field | Description |
|-------|-------------|
| `url` | WebSocket URL (`ws://` or `wss://`) |
| `token` | Auth token (must match server config) |
| `executor_id` | 此执行方的节点身份(executor_id);`relay exec/push/sync/tunnel` 按它路由到该节点 |
| `watch_dir` | Remote directory path (relative to server's watch dir) |
| `command_dir` | Shared directory for config sync commands |
| `executor` | 仅在 `relay node` 的 agent 角色(执行方进程)生效:注册为 `executor_id` 的执行方以接收经中转转发的 `relay execute` / `relay push`。本地 CLI(exec/push/sync)不读取此字段,故同一份配置可与远端共用 |
| `executor_dir` | push 时执行方把文件落到该目录(默认进程当前目录),作为远端项目根;`relay exec` 在本地执行(| Exec 时的工作目录用请求的 cwd) |

**Transparent forwarding (transit hub only):** The transit is a pure transparent forwarder — it **never** executes commands itself. When
`relay exec` is issued locally, the hub forwards it over the existing WebSocket connection to the registered executor (a remote
`relay node` running the agent role with `executor: true`), and relays the command's stdout/stderr back **incrementally** in real time. If no executor is
registered for the watch, `relay exec` fails with `no executor registered for watch '<id>'`.

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
| `config.example.relay.yaml` | Unified config (`server:` + relay backend + workspaces) shared by the transit hub, remote executor and local CLI |
| `config.example.fs-mcp.relay.yaml` | Client config with MCP backend |
| `config.example.jumpserver.relay.yaml` | Client config with JumpServer backend |

The transit (hub) configuration (addr / watch_root / auth / tls) lives in the `server:` section of
`config.example.relay.yaml` — one file drives all three ends, and there is no separate `server.yaml`.

## Config Structure

| Field | Description |
|-------|-------------|
| `name` | Config name |
| `version` | Config schema version (currently 2) |
| `backend` | Backend type + config |
| `workspaces` | List of workspace (job-config) entries |
| `workspaces[].id` | Unique workspace identifier |
| `workspaces[].watch_dir` | Remote directory to monitor |
| `workspaces[].local_dir` | Local working directory for job execution |
| `workspaces[].paths` | Array of glob patterns to match files |
| `workspaces[].jobs` | Actions to execute when file matches |
| `workspaces[].auto_cleanup` | Delete remote file after all jobs succeed (default: false) |
| `workspaces[].executor` | Bind this workspace's jobs to an executor's `executor_id` (empty = single-root fallback) |
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
server:
  watch_root: /data/relay        # each workspace's watch_dir is relative to this
workspaces:
  - id: web-app-patches
    watch_dir: web-app/patches
    ttl: 30m                     # delete files older than 30 minutes
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

### Deletion is via `exec` (no dedicated `file_delete` type)

Jobs only have an `exec` type. To delete files, run a deletion command through
`exec` (use `rm -f` for idempotency — it won't error if the file is already
gone):

```yaml
- id: cleanup_local
  type: exec
  cmd: rm -f {file_path}
  if: jobs.apply.success
```

Two cleanup paths to be aware of:
- **Local copy** (`{file_path}`, the copy synced into `local_dir`): delete it
  with an `exec` job like above.
- **Transit source file** (the file on the backend's `watch_dir`, addressed by
  `{file_remote_path}`): handled automatically by `auto_cleanup: true`, which
  deletes it once all jobs have succeeded. You do not need a delete job for it.

## Built-in Variables

| Variable | Description |
|----------|-------------|
| `{file_path}` | Path to the file synced into `local_dir` (exec jobs run locally) |
| `{file_name}` | Filename without directory |
| `{file_dir}` | Directory containing the locally-synced file |
| `{file_remote_path}` | Original remote path on `watch_dir` (always the remote file, even when a local copy exists) |
| `{timestamp}` | Current time in RFC3339 format |

When a file matches, the watcher first downloads it into `local_dir` (the
backend is a pure file-transfer layer and never executes commands), then runs
the `jobs`/`exec` actions locally on the machine running the executor node. Set
`auto_cleanup: true` to have the watcher remove the remote (`{file_remote_path}`)
copy after all jobs succeed; delete the locally-synced copy with an
`exec` job such as `rm -f {file_path}`.

## Conditional Execution

```yaml
- id: cleanup
  type: exec
  cmd: rm -f {file_path}
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
