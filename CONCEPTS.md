# Concepts

> Shared domain vocabulary for this project — entities, named processes, and status concepts with project-specific meaning. Seeded with core domain vocabulary, then accretes as ce-compound and ce-compound-refresh process learnings; direct edits are fine. Glossary only, not a spec or catch-all.

## Detached execution

### Detach task
A command launched remotely via `relay run -d` (or `exec -d`) that keeps running after the CLI session ends. It is supervised by a TaskSupervisor on the executor, which owns its process group, log, and cleanup, and exposes a task identifier used by `relay ps`, `relay logs`, and `relay stop`. Detaching is how long-running servers survive the 30s foreground cap.

A detach task's stdout/stderr are redirected to its own task log by the executor; the exec session stream itself carries only task-control lines (the task ID hand-off), not the command's output.

### Task ID
A short string (`t-<hash>`) assigned to a detach task at launch, used to address it in `relay ps/logs/stop`. The CLI obtains it at launch so it can later name the task.

## Port forwarding

### Local port-forward
A forwarding rule held by the local `relay node` after a `relay run -d -p` launch: the node listens on the local port, and each inbound connection is pumped over a tunnel to the remote executor's detached task. The node plays the dockerd role — it is the daemon that holds the listen and the port map, and the CLI only registers the rule to it over a unix control socket. The rule is removed when the task is stopped.

## Tunnels

### Node-held tunnel
A SOCKS5 egress endpoint held by the local `relay node` (agent role), created with `relay tunnel start` — the
tunnel counterpart of the local port-forward. The node listens on the local address and pumps each inbound
SOCKS5 connection over a tunnel to the chosen executor's network egress; the executor still validates its own
`network_allow`. The CLI only sends control messages (`open`/`close`/`restart`/`list`) over the unix control
socket `~/.relay/tunnel.sock`.

### Tunnel intent and restart policy
Each tunnel's intent (`egress`, `listen`, `restart`) is persisted to `~/.relay/tunnels.json` — the single
source of truth, `docker run`-style runtime state rather than shared config (so `relay sync` never ships it to
executors). `restart` mirrors `docker run --restart=`; the recovery event is the node's next start, since a
tunnel is a goroutine inside the node process. `always` reopens the listener on node start; `no` (default)
keeps the entry but shows it `stopped` (`relay ps -a`). `relay stop <egress>` is `docker rm` semantics: it
closes and forgets the tunnel.

### Egress identity
A tunnel is addressed by its **egress** — the target executor's `executor_id` — so `relay stop <egress>` and
`relay logs <egress>` name it directly, and `relay ps` merges node-held tunnels (`TYPE tunnel`) with remote
detach tasks (`TYPE task`).

### Egress readiness
The health of a tunnel's far end, taken from the same cluster ledger `relay nodes` reads: `ready` (with
end-to-end RTT), `offline` (registered before, currently disconnected), `notready` (the transit→executor
segment is unavailable), or `unknown` (the ledger could not be queried). It exists because the local listener
is only the first hop of a three-hop chain (local → transit → egress) — it keeps reporting `running` while the
egress is dead, so a proxy request just times out with nothing to look at. Shown in the `relay ps` STATUS
cell and as `egress_status` / `egress_ready` under `--json`.

## RIsk note — marker vs bare ID

The detach task-ID line the executor returns to the CLI is either a marker-prefixed line (new executor, unambiguous, suppressed from user stdout) or a bare ID line (legacy executor, captured via first-non-empty-line fallback). The CLI recognizes both; out-of-band transport of the ID was rejected as stronger CLI/executor version coupling.
