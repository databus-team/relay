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

## RIsk note — marker vs bare ID

The detach task-ID line the executor returns to the CLI is either a marker-prefixed line (new executor, unambiguous, suppressed from user stdout) or a bare ID line (legacy executor, captured via first-non-empty-line fallback). The CLI recognizes both; out-of-band transport of the ID was rejected as stronger CLI/executor version coupling.
