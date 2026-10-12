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

### watch — Daemon Mode

```bash
relay watch -c ~/.relay/config.yaml
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
```

Forwards a command to the remote side for execution. Requires a backend that supports `Exec` (local, fs_mcp, relay). The `-w` flag sets the working directory to the watch's `local_dir`. When omitted, `-w` is inferred from the cwd when it can be; otherwise `run/exec` falls back to its no-workspace behavior. Use `--node / -e <id>` to address a specific node by its ID directly.
- **`-p, --publish [local:]remote`**: Forward local ports to the remote command (e.g. `-p 8080` or `-p 3000:8080`), automatically tearing down the forwarders when the command exits. Temporary development port forwards do not require `network_allow` whitelist rules.
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
TASK ID                 STATUS     PID      UPTIME       COMMAND
t-a1b2c3d4-1718000000   running    12345    12m34s       npm run dev
t-e5f6g7h8-1718000500   stopped    -        -            pytest tests/ (exit: 0)
```

### logs — Stream & Tail Task Logs

```bash
# Live stream task logs (similar to docker logs -f)
relay logs -e node-1 t-a1b2c3d4-1718000000 -f

# View recent lines without following
relay logs -e node-1 t-a1b2c3d4-1718000000 --tail 50
```

Subscribes to live task output broadcast or retrieves persisted log entries from disk.

### stop — Terminate Background Task

```bash
relay stop -e node-1 t-a1b2c3d4-1718000000
```

Stops a running task. The executor cleans up the entire process tree using process groups/signals on Unix or `taskkill /T /F` on Windows, leaving no orphaned server processes behind.

### nodes — List Cluster Topology & Roles

```bash
# List all peer nodes across topology (similar to kubectl get nodes)
relay nodes

# Output in JSON format
relay nodes --json
```

Displays cluster topology with node identities, roles (`hub`, `agent`), operational readiness, platform architecture, and end-to-end latency:
```
NAME                     ROLES          STATUS     VERSION          PLATFORM         LATENCY
CT-MBP.local (local)     agent          Ready      feae97f+feae97f  darwin/arm64     -
hub (transit)            hub            Ready      feae97f+feae97f  linux/amd64      9 ms
extranet                 agent          Ready      feae97f+feae97f  -                12 ms
intranet                 agent          Ready      feae97f+feae97f  -                13 ms
```

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
- The synced file is validated on the executor and written to its `config_path` (default `~/.relay/config.yaml`); `relay watch` picks it up on the next cycle.

**How it works:**
1. CLI reads the local config file and writes it as a command file in the shared `command_dir`
2. Watcher picks up the command, validates the config, creates a backup at `<configPath>.bak`, and stages it
3. New config applies on the next watch cycle (not immediately)

**Requirements:**
- Watcher must be running and started with `-c` flag
- `command_dir` must be consistent between watcher and CLI (default: `/tmp/relay-commands`)
- On validation failure, current config is preserved (no changes made)

### server — Relay Server

Run the relay server on the transit machine.

```bash
# Recommended: the unified config.yaml (three ends share one file)
relay server -c ~/.relay/config.yaml

# Legacy standalone server file
relay server --server-config server.yaml

# With CLI flags only
relay server --addr :8080 --watch tcp:/data/web-app --token secret

# CLI flags override config file values
relay server -c config.yaml --addr :9000
```

**One config, three ends.** The unified `config.yaml` carries a `server:` block (addr / watch_root / auth.tokens / tls) plus the `workspaces` list (each entry is a job-config, able to bind to a specific executor via `executor:`). Each end keeps the whole file and reads only its relevant section. Note that the relay server applies config to how it listens/stores — a change there takes effect on **restart**.

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

The legacy standalone `server.yaml` (top-level `addr`/`watch`/`auth`/`tls`) is still accepted via `--server-config` for backward compatibility. Environment variables (`$VAR`, `${VAR}`) are expanded in all string values.

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
new `relay-linux` via code-server and run `relay server upgrade <path> -c $HOME/.relay/config.yaml`
(or `relay server restart` to just reboot). On a failed upgrade, restore via `.prev` through the
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

```sh
relay tunnel --listen 127.0.0.1:1080 --executor site-a   # egress via executor site-a
curl --socks5-hostname 127.0.0.1:1080 http://intra.a.internal/ping
```

**Daemon mode (like `relay server`/`relay watch`):** run the tunnel as a detached background process so it
survives the launching shell/SSH session, managed per-executor. Foreground (`relay tunnel -w site-a`, the
default) stays unchanged. Control actions:

```sh
relay tunnel -w site-a start      # detached daemon, log ~/.relay/tunnel-site-a.log
relay tunnel -w site-a status     # one instance's status
relay tunnel status               # list ALL tunnel instances (multi-executor) + their pid/log
relay tunnel -w site-a restart    # reboot one
relay tunnel -w site-a stop       # stop one
relay tunnel -w site-a upgrade <bin>   # swap binary & restart one
```

Each executor maps to its own daemon instance (`tunnel-<executor>`, independent pid/log under `~/.relay`),
so tunnels for different egress executors run side-by-side and `relay tunnel status` (no `--executor`)
reports them all at once.

- `--executor` (`-w` alias) selects the egress executor by its node identity **executor_id** (the
  `executors[].executor_id` shown by `relay status --json`) — required. It is passed straight through to the
  transit and routed to the target executor by executor_id (no workspace mapping).
- Default bind is loopback `127.0.0.1`; binding a non-loopback address prints a loud warning (no auth).
- Server must enable the tunnel channel (`tunnel_enabled: true` in the `server:` section); otherwise
  tunnel requests are rejected.
- Optionally bound the concurrent-tunnel cap with `server.max_tunnels` (CLI `--max-tunnels`; default 256).

**Dual-executor / concurrent tunnels (multi-executor):** one transit can host **two (or more)** remote
executors at once, each on its own watch/workspace. Run two `relay tunnel`s in parallel, each binding its
own local port and selecting its own egress — access each executor's intranet targets independently
without interference. Closing one tunnel never affects the others; `relay status` / the version ledger
list every online executor per watch so you can see which egresses are available:

```sh
# executor M1 (watch site-a) and M2 (watch site-b) both online on the same transit
relay tunnel --listen 127.0.0.1:1080 --executor site-a   # egress M1
relay tunnel --listen 127.0.0.1:1081 --executor site-b   # egress M2, independent

curl --socks5-hostname 127.0.0.1:1080 http://a-intra.pvt/00
curl --socks5-hostname 127.0.0.1:1081 https://b-intra.pvt/api
```

**`network_allow` is per-executor.** Each executor's `backend.config` carries the allowlist for the
egress it may reach. In a multi-executor deployment put each executor's own whitelist in that executor's
config (so `relay tunnel --executor site-a` is gated by site-a's allowlist, `--executor site-b` by site-b's).
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

## Relay Backend Configuration

When using the `relay` backend, configure the WebSocket connection to the relay server:

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
| `executor` | 仅在 `relay watch`(执行方进程)生效:注册为 `executor_id` 的执行方以接收经中转转发的 `relay execute` / `relay push`。本地 CLI(exec/push/sync)不读取此字段,故同一份配置可与远端共用 |
| `executor_dir` | push 时执行方把文件落到该目录(默认进程当前目录),作为远端项目根;`relay exec` 在本地执行(| Exec 时的工作目录用请求的 cwd) |

**Transparent forwarding (transit server only):** The relay server is a pure transparent forwarder — it **never** executes commands itself. When
`relay exec` is issued locally, the server forwards it over the existing WebSocket connection to the registered executor (the remote
`relay watch` with `executor: true`), and relays the command's stdout/stderr back **incrementally** in real time. If no executor is
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
| `config.example.relay.yaml` | Unified config (`server:` + relay backend + workspaces) shared by transit server, remote executor and local CLI |
| `config.example.fs-mcp.relay.yaml` | Client config with MCP backend |
| `config.example.jumpserver.relay.yaml` | Client config with JumpServer backend |

The relay server configuration (addr / watch_root / auth / tls) lives in the `server:` section of
`config.example.relay.yaml` — one file drives all three ends. No separate `server.yaml` is needed.

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
the `jobs`/`exec` actions locally on the machine running `relay watch`. Set
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
