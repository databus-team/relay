# PROJECT KNOWLEDGE BASE

**Generated:** 2026-09-02 (rewritten against current code — supersedes the 2026-05-26 edition)

## OVERVIEW

Go-based file exchange & remote command execution system, built around a **star topology with a single transit relay server (中转)**:

```
本机 macOS ──> relay server (transit, Linux) <── 远端 executors (Windows/Linux, NAT 后)
```

- The transit server is the only network intersection point; all traffic (push / exec / sync / tunnel / status) flows through it over WebSocket.
- Executors are the `relay watch` processes that **self-register** on the transit and execute jobs / answer exec / host tunnels.
- A single shared `config.yaml` drives all three ends (transit reads `server:`, executor reads `backend:`+`workspaces:`, CLI reads `backend:`+`workspaces:`).
- Event-driven for the relay backend (fsnotify upstream, WS push downstream); polling fallback for local/fs-mcp/jumpserver.

Stack: Go 1.25, kingpin (CLI), gorilla/websocket, yaml.v3, zstd (protocol compression), fsnotify.

## STRUCTURE

```
relay/
├── cmd/relay/                 # CLI entry: all subcommands (server/status/watch/push/...)
│   ├── main.go               # kingpin wiring + most run*() handlers (server/sync/ws/version/job/status...)
│   ├── server.go             # runServer + transit self-upgrade injection
│   ├── tunnel.go             # SOCKS5 tunnel plane: create/control plumbing + SOCKS5 handshake & conn pump
│   └── tunnel_holder.go      # Node-resident tunnel holder (node role): listen + open/close/restart/list over ~/.relay/tunnel.sock
├── internal/
│   ├── relay/
│   │   ├── server/           # Transit WebSocket server: routing, executor registry, upgrade, tunnel, fs watcher
│   │   ├── client/           # Relay client: dial/reconnect/heartbeat/stream/tunnel/connection pool
│   │   ├── protocol/         # Wire MessageType + message structs + zstd compression
│   │   └── backend/          # `relay` backend (executor-side role): register/exec/tunnel/network_allow
│   ├── backend/              # FileTransferBackend interface + local/fs-mcp/jumpserver implementations
│   ├── config/               # YAML config: workspaces(+executor binding) + server block + tunnel_allow
│   ├── watcher/              # Executor/watch daemon: event-driven or polling, job chains, config hot-reload
│   ├── exchange/             # cmd/result file protocol (shared-dir exchange for non-relay backends)
│   ├── jobrunner/            # Run config-defined jobs locally (job run, push-triggered)
│   ├── portfwd/              # Canonical port-forward spec + parse grammar, shared by CLI/protocol/backend/supervisor
│   ├── daemon/               # Daemon lifecycle (pid/log) + binary self-replace/upgrade
│   ├── logx/                 # Debug logging toggle
│   └── version/              # Build version stamp (set via ldflags)
├── scripts/                  # relay-deploy.sh (deploy-remote/transit), command-responder.sh
├── docs/                     # plans/, brainstorms/, relay-protocol.md, relay-positioning.md
├── config.example.relay.yaml # One shared config for all three ends
├── Makefile
└── README.md
```

## WHERE TO LOOK

### CLI / cmd

| Task | Location | Notes |
|------|----------|-------|
| All subcommands & dispatch | `cmd/relay/main.go` | kingpin wiring; one `run*()` per command |
| `server` (transit) | `cmd/relay/server.go` | `runServer()`, legacy vs unified config, `transitSelfUpgrade` |
| `run` (alias `exec`) | `cmd/relay/main.go` → `runExec` | Creates both object kinds: a task (default) or, with `-t tunnel`, a node-held SOCKS5 tunnel |
| Tunnels (SOCKS5) | `cmd/relay/tunnel.go` + `cmd/relay/tunnel_holder.go` | Created by `relay run -t tunnel`; CLI sends control (open/close/restart/list) over `~/.relay/tunnel.sock`; SOCKS5 handshake, egress executor via `-e`/`--executor` |
| `status` | `cmd/relay/main.go` → `runStatus`/`printStatus` | Deployment-wide latency + version ledger |
| `ws` (alias `workspaces`) | `runWorkspaces`/`resolveWatches` | List/JSON/verbose workspace table |
| `sync` (`-e` target executor) | `runSync` | WS streaming to executor, or cmd-file fallback |
| `push` (direct + `--no-jobs`/`--dest`) | `runPush` · `runLocalJobsForPush` | Direct-to-executor with jobs, or transport-only |
| `exec` / `run` | `runExec` | Streaming via `ExecStreamBackend`; git diff sync **default-on** (`--no-sync` to skip, `-f` to force); `-d` (detach); `-t tunnel` creates a tunnel instead; 30s foreground cap |
| `ps` | `runPs` | List managed tasks (RESTART policy, first-created CREATED) and node-held tunnels across nodes (a tunnel row's CREATED is its last bind) |
| `logs` | `runLogs` | Tail / stream task or tunnel logs (`-f`, `--tail`) |
| `stop` | `runStop` | Terminate a task process group (and clear its restart policy) or close+forget a tunnel |
| `start` / `restart` | `runStart` / `runRestart` | Root lifecycle verbs by name (task id or tunnel egress); `start` never creates and never restarts a running object |
| `undo` | `cmd/relay/git_ops.go` | Restore remote workspace shadow snapshot |
| `diff` | `cmd/relay/git_ops.go` | Compare local/remote Git drift & conflict warning |
| `job run` | `runJobRun` | Run one config job locally |
| Version stamp | `internal/version/version.go` | `Version`/`Commit`/`Date`, `String()`, `Full()` |

### relay server (transit)

| Symbol | Location | Role |
|--------|----------|------|
| `Server` / `New` / `Serve` | `internal/relay/server/server.go:23/114/228` | WS upgrade serving, connection handling |
| `handleConnection` | `server.go:271` | Per-conn read/write loops |
| `RegisterExecutor` / `UnregisterExecutor` | `server.go:405/421` | **self-registration** of executors by executor_id (replaced stale) |
| `GetExecutor` / `ExecutorEndpoints` / `ExecutorVersions` | `server.go:442/457/431` | Executor routing + version ledger |
| `SendTo` / `Subscribe` / `BroadcastToSubscribers` | `server.go:360/372/657` | Message routing + file-event fan-out |
| `SetUpgradeSwap` | `server.go:224` | Injected transit self-upgrade swap closure |
| Tunnel registry (`TunnelEnabled`, `maxTunnels`=256) | `server.go:537/86`, `registerTunnel` | Gated `MsgTunnel*` egress |
| `FileWatcher` (fsnotify) | `internal/relay/server/watcher.go:16` | Emits `MsgFileEvent` to subscribers |
| `Client` inbound handlers | `internal/relay/server/client.go` | exec/list/delete/push/pull/exec_sync/status/upgrade/tunnel |

### relay client

| `Client` / `New` / `Connect` | `internal/relay/client/client.go:16/71/95` | WS transport, message dispatch |
| Reconnect backoff | `internal/relay/client/reconnect.go:9` | default 1s→30s ×10, doubling |
| Heartbeat | `internal/relay/client/heartbeat.go:12` | every **30s**; timeout if last pong > **90s** → disconnect; generation-tracked |
| `Request` / `sendAndWait` | `client.go:365/394` | Request/response + pending map |
| `RegisterExecutor` | `client.go:431` | Executor self-registration handshake |
| `Status` / `Version` / `Ping` | `client.go:466/447/551` | `relay status` / version catalog / liveness |
| Stream (`Pull`/`Push`) | `client.go:27` (`stream.go`) | Chunked binary over WS |
| Exec / ConfigSync / PushJob / Transport | `client/exec.go` | Streaming exec, config push, push jobs, send |
| Tunnel | `client/tunnel.go` | `TunnelOpen`/`TunnelStream`, fail-closed on backlog/transport loss |
| Connection pool | `client/pool.go` | `GetOrConnect` keyed url|token|watch|headers → multi executor |

### relay protocol

| `MessageType` + all `Msg*` | `internal/relay/protocol/message.go:4-32` | Wire message tags |
| Message structs | `message.go` | Connect/List/Exec/Push/Pull/PushJob/ConfigSync/Status/Tunnel... |
| `StatusResponse`/`StatusSegment` | `message.go:181/159` | Transit + per-executor latency segments |
| `Compress`/`Decompress` | `protocol/compress.go` | zstd, binary WS frames |

### relay backend (executor side)

| `RelayBackend` / `NewRelayBackend` | `internal/relay/backend/relay.go:29/89` | `backend.type=relay` file/exec interface |
| `SetExecutorRole` | `relay.go:63` | `relay watch` sets executor role (CLI doesn't) |
| `registerExecutor` | `relay.go:553` | Self-register executor_id on connect |
| `Exec` / `ExecStream` / `PushJob` / `PushNoJobs` / `Transport` | `relay.go:239-312` | Inbound exec/push on executor |
| `ConfigSync` | `relay.go:336` | Stream config onto executor disk |
| `UpgradeServer` | `relay.go:349` | Transit self-upgrade stream + verify + ACK |
| `TunnelOpen` / `handleInboundTunnelConnect` | `relay.go:327/392` | SOCKS egress; validates `network_allow` |
| `Events` / `SubscribeEvents` | `relay.go:735/739` | Relay-driven (event) watch channel |
| registry `init` | `relay.go:780` | `backend.RegisterBackend("relay", ...)` |

### config (workspaces + executor + server)

| `Config` struct | `internal/config/config.go:15` | `Backend` + `Workspaces[]` + `Server` + `Interval` |
| `WorkspaceConfig` | `config.go:47` | `ID/WatchDir/LocalDir/Paths/Jobs/AutoCleanup/TTL/Executor` |
| `Executor` binding | `config.go:55` | binds a workspace's jobs to one executor by its `executor_id`; empty = single-root |
| `ServerConfig` | `config.go:26` | `Addr/WatchRoot/Auth/TLS/TunnelEnabled/MaxTunnels` |
| `GetWorkspaceByID` | `config.go:165` | lookup a single workspace |
| `GetWorkspacesByExecutor` | `config.go:176` | all workspaces bound to an executor `executor_id` |
| `ApplyConfigFile` | `config.go:138` | validate + backup + **atomic tmp/rename** config write |
| `Load`/`LoadFromBytes` | `config.go:89/105` | YAML load with env expansion + home expand |
| `NormalizeWindowsPath` (MSYS→`D:\`) | `config.go:186` | Windows path normalization |
| `ParseNetworkAllowlist`/`CheckTunnelTarget` | `internal/config/tunnel_allow.go:53/126` | tunnel egress allow rule engine |

### watcher

| `Watcher` struct | `internal/watcher/watcher.go:25` | cfg, configPath, processed, pendingConfig |
| `New` / `Run` | `watcher.go:40/60` | factory per-workspace backend; event-driven (`runEventDriven` for relay) or ticker |
| `runEventDriven` | `watcher.go:106` | relay-backend event loop |
| `processCommands` / `processCommandsLoop` | `watcher.go:352/239` | command file polling (adapts 2s→30s) for non-relay backends |
| `heartbeat` | `watcher.go:275` | shared-store `.heartbeat` every 5s, **skipped for relay** executor |
| `handleConfigSync` / `applyPendingConfig` | `watcher.go:457/530` | staged config hot-reload |
| `processWatch` / `executeJobs` | `watcher.go:599/662` | glob-match + job chain with `if:` gating |
| `SubstituteVariables` | `watcher.go:789` | `{file_path}`/`{file_name}`/`{file_dir}`/`{file_remote_path}`/`{timestamp}` |
| `cleanupProcessedMap` | `watcher.go:344` | caps `processed` (#threshold) |

### backend / exchange / daemon / jobrunner

| `FileTransferBackend` interface | `internal/backend/backend.go:22` | `ListDir/Read/Write/Delete/SupportsExec/Exec/Ping`; `ErrNotSupported` |
| Backend registry | `backend.go:92/96` + `init()` per backend | `local`, `fs-mcp`, `jumpserver`, `relay` |
| Optional capability interfaces | `backend.go:33-84` | `EventBackend`, `ExecStreamBackend`, `PushJobSender`, `PushNoJobsSender`, `ConfigSyncCapable`, `PushJobHandler` |
| `local.go` / `fs_mcp.go` / `jumpserver.go` | `internal/backend/` | local FS, MCP-over-SSE (no exec support), JumpServer REST (no exec support) |
| Cmd/result protocol | `internal/exchange/cmdfile.go` | JSON over shared dir (fallback path for non-relay backends, `config_sync` cmd-file) |
| Daemon lifecycle | `internal/daemon/*.go` | start/stop/restart, `PidFile`, self-replace, `ReplaceBinaryWithKeep` |
| Local job runner | `internal/jobrunner/jobrunner.go` | `Run`/`RunJobs` with condition slots |
| `portfwd.Spec` (CLI alias `PortForwardSpec`) | `internal/portfwd/spec.go` | Canonical port-forward spec + `Parse`/`String` grammar shared by CLI/protocol/backend/supervisor (KTD9) |
| `TaskMeta` (`Restart`/`CreatedAt`) | `internal/supervisor/task.go:29` | Persisted task metadata: restart policy + first-created time, preserved across restarts |

## COMMANDS

Build/deploy targets in `Makefile` (all stamp version via `internal/version`):

```bash
make build           # dev binary (./relay)
make build-release   # CGO_ENABLED=0, stripped (-s -w) + version stamp
make build-linux     # cross-compile Linux amd64 → relay-linux (stamped)
make build-windows   # cross-compile Windows amd64 → relay.exe (stamped)
make build-debug     # debug symbols (-gcflags -N -l)
make test            # go test -v -race ./...
make test-coverage   # coverage.out + html
make clean / fmt / vet / deps / run
make install         # build-release → ~/.local/bin/relay
make deploy-remote   # scripts/relay-deploy.sh remote: all online executors (RESTART=1 to swap+restart; RELAY_EXECUTORS to narrow)
make deploy-transit  # scripts/relay-deploy.sh transit  (controlled self-upgrade)
make deploy          # all (remote + transit)
make help
```

CLI usage (key forms):

```bash
relay node [run|start|stop|status|restart]             # unified peer node daemon (hub + agent roles)
relay watch [run|start|stop|status|restart|upgrade]   # executor (daemon actions)
relay server [run|start|stop|status|restart|upgrade]  # transit (daemon actions)
relay run [-w <workspace> | --node <node-id>] [-p <port>] [--no-sync] [-f] [-d [--restart=always|no]] <cmd...> # direct run / exec (Docker-style -p; --restart/--no-restart requires -d)
relay run -t|--type=tunnel -p <[host:]port> -e <egress> [--restart=always|no]  # create a node-held SOCKS5 tunnel (no command, no -d/-w)
relay exec [-w <workspace> | --node <node-id>] [-p <port>] [--no-sync] [-f] [-d] <cmd...>  # git diff sync default-on; -d = detached (30s foreground cap)
relay nodes [-u|--usage] [--json] [--prune-offline]   # cluster-wide peer nodes & topology state; shows known-offline nodes, --prune-offline drops them (alias: node ls, status)
relay push [-w <workspace> | --node <node-id>] <file> [--no-jobs] [--dest <abs>]
relay ps [--node <node-id>] [-a]                       # list tasks (TaskSupervisor) + node-held tunnels; RESTART policy column, CREATED = first-created (task) / last bind (tunnel)
relay logs [--node <node-id>] <task-id|egress> [-f] [--tail N] # tail / stream task or tunnel logs
relay stop [--node <node-id>] <task-id|egress>         # terminate task process tree (clears its restart policy) or close+forget a tunnel
relay start [--node <node-id>] <task-id|egress>        # reopen an existing task/tunnel (never creates one)
relay restart [--node <node-id>] <task-id|egress>      # stop the live instance (if any) then reopen it
relay diff [--node <node-id>] [-w <workspace>]         # inspect drift & conflict warning
relay undo [--node <node-id>] [-w <workspace>] [--list|--snapshot <ref>] # restore remote snapshot
relay job run [-w <id>] <jobid> [file]
relay sync  [--node <node-id>]
relay ws [-v] [--name <id>]                            # alias: relay workspaces
relay version [--json]                                 # local build version
relay server-remote [--binary <path>] [--expect <ver+commit>]  # one-command transit self-upgrade
# Tunnels have no sub-command namespace: create with `relay run -t tunnel`, operate with the root verbs above
# (`relay ps`/`start`/`restart`/`stop`/`logs` address a tunnel by its egress; TYPE `tunnel` rows, `[tunnel <egress>]` log prefix).
relay completion <bash|zsh|fish|powershell>             # generate shell completion script
relay pull [-w <workspace>] <filename> [-dl]    relay list [-w]    relay cleanup [-w <workspace>]
```

Pass `-c <config>` (default `~/.relay/config.yaml`) to share one config across ends; `relay sync` ships it to the executor.

## IMPLEMENTATION NOTES (current)

- **Node addressing ⊥ job routing.** Layering split: `executor_id` = an executor's node identity (its `backend.config.executor_id`), used for registration and as the routing key for exec/push/sync/tunnel/status; `watch`/`watch_dir` = a transit storage directory (id = a workspace id). `relay exec`/`push` accept either `-w <workspace>` (route to that workspace's bound executor) or `-e/--executor <executor_id>` (address a specific node directly, no workspace binding needed); a tunnel takes only `-e/--executor` (there is no workspace half). A running executor with no bound workspace (e.g. extranet) is still addressable via `--executor`, which is what the deploy script uses.
- **Push direct + fallback.** On `relay push`, the relay backend uses `PushJobSender` to fan the file directly to the target executor through the transit (never the shared-dir → staged by the transit), and the executor runs its workspace jobs locally, streaming the job output back. `--no-jobs`/`--dest` uses `PushNoJobsSender` (transport-only). If no online executor is registered for that workspace (`executor: none`), the push **falls back** to staging the file on the transit watch dir (watch-pull) — this fallback is verified in `TestEndToEnd_PushJobNoExecutorFallback`.
- **Tunnel `network_allow` fail-closed.** The **local `relay node` (agent role)** holds the SOCKS5 listener and pumps each CONNECT to the chosen executor via the transit; the tunnel is **created by `relay run -t tunnel`** and thereafter operated by the **root lifecycle verbs** (`relay start|restart|stop|logs <egress>`), which send open/close/restart control messages over `~/.relay/tunnel.sock`, while reads go through `relay ps` (TYPE `tunnel` rows, backed by the same socket's `list`; the row's STATUS also carries egress reachability from the cluster ledger, degrading to `unknown` when the transit can't be queried) and `relay logs <egress>`; the tunnel intent (egress/listen/restart) is persisted to `~/.relay/tunnels.json` for replay on node restart (`--restart=always|no`). The **executor** validates the destination against its `backend.config.network_allow` (host/IP/CIDR + optional `@port` list/range). Unlisted targets are rejected by default (fail-closed); an empty/absent `network_allow` denies all. Server gates the channel behind `server.tunnel_enabled` + `max_tunnels` (default 256). Non-loopback listen prints a warning (no auth on the tunnel itself).
- **`nodes` 拓扑与资源发现 (轻量默认 + `--usage` 动态探测).** `relay nodes` (别名 `status`) 统一呈现全网拓扑、节点角色、平台架构、资源规格与网络 RTT：
  - **容器与 Pod 隔离感知**：针对 Hub 运行于 K8s Pod 场景，自动适配 cgroup v1/v2，精准获取容器配额（`cpu.max`/`cfs_quota_us` 与 `memory.max`/`limit_in_bytes`），避免误读宿主机物理资源，并追加 `[pod]` 标识；
  - **轻量低开销**：默认列仅显示静态 CPU 核心配额与内存限制（注册与本地秒级解析，零网络重负）；
  - **`-u, --usage` 按需动态探测**：在中转向各执行方发送往返探针计算 RTT 的同一周期内，并发收集并回传执行方与 Hub 的动态 CPU Load 与已用内存及百分比。
- **Heart beats 30s/90s.** The relay client sends a WS heartbeat every 30s; if no pong arrives for >90s it disconnects and lets reconnect take over (heartbeat is generation-guarded so only the current connection's ticker runs). The legacy shared-store `.heartbeat` (5s) is **skipped** by the relay executor — liveness there is carried by the relay connection + self-registration, not a shared file.
- **Reconnect 1s→30×10.** Default `reconnect.go` config: initial 1s, exponential ×2 up to 30s max, max 10 retries, then gives up (`failAllPending` + close). A successful reconnect restarts `readLoop` + heartbeat (single writeLoop persists).
- **Config atomic write.** `ApplyConfigFile` parses/validates the payload, backs up the current file to `config.bak`, writes to `config.tmp`, then `os.Rename` (atomic). It is shared by both sync paths: the exchange/`command`-file watcher (`handleConfigSync`) and the relay WS streaming `ConfigSync`. `relay sync` pushes the shared config to the executor.
- **Streaming/interaction.** `MsgExec*`/push/stream transfer binary content via `StreamStart`/`StreamData`/`StreamEnd` (binary frames + zstd) rather than in JSON headers; file events fan out via `MsgFileEvent` to subscribers.
- **Exec/task output is byte-chunked, not line-chunked.** `runStream` (`internal/relay/backend/relay.go`) and the supervisor's `pipeToLog` (`internal/supervisor/task.go`) emit raw byte blocks (≤ `streamutil.ChunkSize`) so `\r` progress output and newline-less output stream immediately; a boundary may split a multi-byte rune, which `streamutil.SplitIncompleteRune` holds back. `Task.StreamLogs` follows **by file offset in `run.log`** (notify + 200ms poll fallback), so a slow follower lags but never loses lines and never back-pressures the task; a saturated in-stream channel (`client.routeExecStream`) waits up to 5s instead of dropping frames.
- **`relay ps` 的出口探测是「定点 + 有预算」的.** The status probe (`MsgStatus`) answers only after the **slowest** executor's `MsgPing`→`MsgPong` returns, and that pong shares the executor's send queue with tunnel/stream data — a hammered executor looks "online but probe-timeout". So `StatusRequest` carries two optional narrowing fields: `ExecutorIDs` (probe only these) and `ProbeTimeoutMS` (per-probe wait). `relay nodes` sends neither (wants the whole ledger, tolerates 5s); `relay ps` sends only its running tunnels' egress ids with an 800ms budget (`psEgressProbeTimeout`), so unrelated slow nodes can't drag it and a slow egress yields an honest `notready` instead of `unknown`.
- **Exec git-sync semantics + 30s foreground cap.** `relay run`/`exec` sync the local Git diff **by default** — there is no `--sync` flag; `--no-sync` opts out and `-f`/`--force-remote` forces it. Two consequences: (a) sync carries **only uncommitted working-tree changes, never commits** — unpushed commits do not transfer; (b) its source dir is the workspace's `local_dir`, which must be a valid Git repo **on the CLI machine** — a Windows-style `local_dir` (e.g. `/d/...`) is invisible to a macOS CLI, so sync is silently skipped. A foreground exec is capped at **30s** on the executor (`runStream` defaults `timeout=30` when the client sends 0; no CLI override), so long commands/servers must use `-d` (TaskSupervisor) + `relay logs <task-id> -f`.

## CONVENTIONS

- **Standard Go layout**: `cmd/` for the binary, `internal/` for packages.
- **Config is unified**: one shared `config.yaml` per `relay server`, `relay watch`, `relay node`, `relay push`; only the relevant section is read on each side.
- **Peer / Node Architecture**: `node:` block configures node identity (`node_id`), roles (`roles: [hub, agent]`), and access control (`authorized_nodes`). `relay node` daemon manages both roles within one process without port conflict.
- **Backend registry**: constructor `init()`-based `RegisterBackend("name", NewX)`; `NewBackend(type, cfg)` dispatch, no switch.
- **Error handling**: `fmt.Fprintf(os.Stderr, ...)` + `os.Exit(1)` in `main`; internal returns errors.
- **Windows compat**: MSYS-style normalization (`/d/...` → `D:\...`) via `NormalizeWindowsPath`; `relay/backend` has `msysToWindowsPath` for exec dirs.
- **Testing**: standard `testing`, table-driven, `_test.go` beside source; backend/integration tests in `internal/relay/backend/relay_test.go` spin up an in-process hub via `httptest`.
- **Bilingual code**: ID-agnostic; comments frequently Chinese, wire types English.

## ANTI-PATTERNS

- **No global state** beyond the backend registry (`backends` var), the relay executor-role flag (`SetExecutorRole`), and the process-level singletons that must be unique *per process*: `supervisors`/`supervisorBaseDir` (one supervisor per task directory) and `replayMu`/`replayedDir` (task replay happens once per process, R6/KTD4) in `internal/relay/backend/relay.go`, plus the CLI's `nodeDaemonRunning` test seam in `cmd/relay/main.go` (a mutable function pointer that tests swap out).
- **No pooling surprises**: Worker pool is keyed by url+token+watch+headers and only reused when connected — reuse of a dead client is re-connected, not implicitly refreshed.
- **Do not send binary/payload in JSON headers** — use stream messages + zstd.
- **Do not assume a target executor is online**: egress/exec/push must degrade cleanly (the return fallback) instead of erroring in a way that drops the file.

## NOTES

- `processed` map is bounded by `cleanupProcessedMap` (reset is lossy; files may re-trigger after a clear).
- Config hot-reload is staged (synced then applied on next cycle / next event), never immediate.
- Watcher in non-relay mode is polling; the relay backend is **event-driven** (no polling).
- CLI commands automatically leverage adaptive loopback (`getBackendWithAdaptiveLoopback`) to connect directly to loopback when run on a machine acting as hub.
- Module path is `github.com/user/relay` — update before publishing.
- Docs: `docs/relay-protocol.md` (wire spec), `docs/relay-positioning.md` (arch rationale / why-not-alternatives), latest plans under `docs/plans/`.