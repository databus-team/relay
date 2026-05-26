---
title: feat: Config sync hot reload
type: feat
status: completed
date: 2026-05-26
origin: docs/brainstorms/2026-05-26-config-sync-requirements.md
---

# feat: Config sync hot reload

## Overview

Add a `sync` command that pushes config content over the existing command-file exchange channel and triggers an in-process reload on the watcher. The watcher validates and backs up the current config, applies the new config on the next cycle, and returns a success/failure result.

## Problem Frame

The watcher only loads config at startup. Operators need a safe, in-process hot reload path that can update `config.yaml` without stopping the watcher and with rollback safety (see origin: docs/brainstorms/2026-05-26-config-sync-requirements.md).

## Requirements Trace

- R1. Provide explicit `sync` command to send new config via command exchange and trigger hot reload.
- R2. `sync` waits for watcher success/failure and surfaces error details.
- R3. Sync applies to the global config (all watch entries).
- R4. Watcher intercepts built-in config-sync command and reloads in-process.
- R5. Reload does not interrupt running jobs; new config applies next cycle.
- R6. Validate config before apply; on failure, keep current config.
- R7. Create a single overwrite-style backup before applying new config.

## Scope Boundaries

- No per-watch config overrides.
- No UI or external management service.
- No change to job execution semantics beyond config reload timing.

## Context & Research

### Relevant Code and Patterns

- CLI command wiring: `cmd/relay/main.go`
- Command exchange protocol: `internal/exchange/cmdfile.go`
- Watcher loops and command processing: `internal/watcher/watcher.go`
- Config load and normalization: `internal/config/config.go`
- Backend abstractions and support matrix: `internal/backend/backend.go`, `internal/backend/local.go`, `internal/backend/fs_mcp.go`, `internal/backend/jumpserver.go`
- Command responder protocol compatibility: `scripts/command-responder.sh`
- Prior protocol design notes: `docs/brainstorms/patch-transfer-system.md`, `docs/plans/2026-05-25-001-feat-file-exchange-system-plan.md`

### Institutional Learnings

- None found (no `docs/solutions/` in repo).

### External References

- Not used (local patterns are sufficient).

## Key Technical Decisions

- **Protocol extension:** Add optional `op` and `payload` fields to `exchange.CmdFile`, using a reserved op value (e.g., `relay:config-sync`) with base64 payload for config content. This preserves backward compatibility and avoids shell-escaping fragility.
- **Config path ownership:** The watcher stores the `-c` config path at startup and uses it as the write/backup target. If missing or unwritable, sync returns a failure result.
- **Reload timing:** Apply pending config only between cycles (before the next `runOnce`), ensuring in-flight jobs are not interrupted.
- **Backup location:** Single overwrite-style backup stored as `<configPath>.bak`.
- **Command directory:** Sync resolves `command_dir` from backend config when present; otherwise defaults to `/tmp/relay-commands` to match the watcher’s default. Document that `command_dir` must be consistent across watcher and CLI for non-default backends.

## Open Questions

### Resolved During Planning

- **How to carry config content:** Use `CmdFile.op` + `CmdFile.payload` (base64). No additional files beyond existing cmd/result files.
- **Backends without Exec:** Sync uses the file exchange (Read/Write/Delete), so it can work with backends lacking `Exec` as long as the watcher is running and can read the command dir. No preflight `Ping` requirement when unsupported.
- **Missing config path:** Return a failed result and leave config unchanged.

### Deferred to Implementation

- **Payload size limits:** Decide whether to enforce a max payload size and how to surface oversize errors.
- **Timeout policy:** Whether to expose a `sync --timeout` flag or keep defaults from `exchange.FileExchange`.

## High-Level Technical Design

> *This illustrates the intended approach and is directional guidance for review, not implementation specification. The implementing agent should treat it as context, not code to reproduce.*

```mermaid
sequenceDiagram
  participant CLI as relay sync
  participant Backend as FileTransferBackend
  participant Watcher as watcher

  CLI->>Backend: write cmd-<id>.json (op=relay:config-sync, payload=base64 config)
  Watcher->>Backend: read cmd-<id>.json
  Watcher->>Watcher: validate config; backup <configPath>.bak; write new config
  Watcher->>Watcher: stage pending config for next cycle
  Watcher->>Backend: write result-<id>.json (exit_code/stdout/stderr)
  CLI->>Backend: poll result-<id>.json and report success/failure
```

## Implementation Units

- [ ] **Unit 1: Extend command-file protocol for config sync**

**Goal:** Enable cmd files to carry a built-in op + payload while remaining backward compatible.

**Requirements:** R1, R2

**Dependencies:** None

**Files:**
- Modify: `internal/exchange/cmdfile.go`
- Create: `internal/exchange/cmdfile_test.go`

**Approach:**
- Add optional `Op` and `Payload` fields to `CmdFile` JSON struct.
- Keep existing `cmd/cwd/timeout` semantics unchanged for normal exec.
- Add helper for building config-sync commands (op value + payload encoding).
- Ensure serialization works with existing responders (extra fields ignored).

**Patterns to follow:**
- Existing cmd/result JSON handling in `internal/exchange/cmdfile.go`.

**Test scenarios:**
- Happy path: marshal/unmarshal `CmdFile` with `op` + `payload` preserves fields.
- Edge case: legacy cmd file without new fields still parses correctly.

**Verification:**
- Existing exec flow unchanged; cmd file with `op` + `payload` serializes as expected.

- [ ] **Unit 2: Watcher config reload handler**

**Goal:** Intercept config-sync commands, validate and stage new config, and apply it on the next cycle with backup.

**Requirements:** R4, R5, R6, R7

**Dependencies:** Unit 1

**Files:**
- Modify: `internal/watcher/watcher.go`
- Modify: `internal/config/config.go`
- Create: `internal/watcher/watcher_test.go`

**Approach:**
- Add a `configPath` string field to the `Watcher` struct and thread it through `New()` from `cmd/relay/main.go runWatch()`. The watcher needs the config file path (not just the parsed config object) to write the backup and new config file.
- Add `LoadFromBytes(data []byte) (*Config, error)` to `internal/config/config.go` — extract the YAML unmarshal, interval default, and path normalization logic from `Load()` into a shared internal helper so both `Load()` and `LoadFromBytes()` use it. The watcher needs this to validate config content from the command payload before writing it.
- In `processCommands`, detect `op=relay:config-sync` and handle internally (no shell execution).
- Validate config (YAML parse via `LoadFromBytes` + minimal structural checks), write backup `<configPath>.bak`, write new config file, and stage pending config for next cycle.
- Apply pending config before each `runOnce` tick — clarify whether this uses in-memory staging with mutex or filesystem atomicity (`os.Rename` over a temp file). The current code re-reads the file on disk each cycle; document the chosen swap mechanism.
- Return result file with clear stderr on validation or write failures.

**Patterns to follow:**
- Command processing loop and result writing in `internal/watcher/watcher.go`.
- Config normalization in `internal/config/config.go`.

**Test scenarios:**
- Happy path: valid payload stages config and results in success output; pending config applies on next cycle.
- Error path: invalid YAML returns failure result; no backup written; config unchanged.
- Error path: missing/unwritable config path returns failure result.
- Edge case: reload queued while jobs executing does not interrupt current cycle.

**Verification:**
- Config changes become effective only on next cycle; error cases leave config unchanged.

- [ ] **Unit 3: CLI `sync` command**

**Goal:** Add a CLI command that pushes local config content and waits for watcher result.

**Requirements:** R1, R2, R3

**Dependencies:** Unit 1

**Files:**
- Modify: `cmd/relay/main.go`

**Approach:**
- Add `sync` subcommand using the existing `-c/--config` path as the local source.
- Use `exchange.FileExchange` with resolved `command_dir` to write a `CmdFile` containing `op=relay:config-sync` and payload (base64 config).
- Poll for result and exit non-zero on failure, surfacing stderr.
- If backend does not support `Ping`, skip preflight and rely on result timeout.

**Patterns to follow:**
- Existing `exec` command flow in `cmd/relay/main.go`.
- File exchange polling in `internal/exchange/cmdfile.go`.

**Test scenarios:**
- Happy path: CLI prints success when watcher returns `exit_code=0`.
- Error path: CLI surfaces watcher stderr and exits non-zero.
- Error path: command dir write/read failures produce clear errors.

**Verification:**
- `sync` reports success/failure deterministically and waits for watcher result.

- [ ] **Unit 4: Documentation updates**

**Goal:** Document the new sync behavior, requirements, and limitations.

**Requirements:** Supports all (user-visible guidance)

**Dependencies:** Unit 3

**Files:**
- Modify: `README.md`
- Modify (if applicable): `config.example*.yaml` to show `command_dir` alignment

**Approach:**
- Add `sync` usage and example.
- Document that watcher must be started with `-c` and `command_dir` must match between watcher and CLI.
- Document backup location `<configPath>.bak` and next-cycle apply semantics.

**Test scenarios:**
- Test expectation: none — documentation only.

**Verification:**
- README reflects new command and operational constraints.

## System-Wide Impact

- **Interaction graph:** CLI `sync` → command-dir files → watcher command loop → config file write → in-memory config swap.
- **Error propagation:** Validation or filesystem errors return through result file and bubble to CLI.
- **State lifecycle risks:** Pending config must swap atomically between cycles; avoid mid-cycle mutations.
- **API surface parity:** New CLI command and additional cmd-file fields; ensure legacy responders ignore extra fields.
- **Integration coverage:** End-to-end sync requires watcher running and consistent `command_dir`.
- **Unchanged invariants:** Job execution semantics and watch processing behavior remain unchanged aside from config source update.

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| Command dir mismatch between watcher and CLI | Document and default to `/tmp/relay-commands`; encourage explicit `command_dir` in config |
| Payload size too large for transport | Define/validate max size; surface clear error (deferred) |
| Missing or unwritable config path on watcher | Fail fast with error result; require `-c` at watcher startup |

## Documentation / Operational Notes

- Sync requires a running watcher that owns the config path (`-c`).
- Backup file stored as `<configPath>.bak` and overwritten on each successful sync.
- `command_dir` must be consistent between watcher and CLI for sync to work.

## Sources & References

- **Origin document:** docs/brainstorms/2026-05-26-config-sync-requirements.md
- Related code: `cmd/relay/main.go`, `internal/watcher/watcher.go`, `internal/exchange/cmdfile.go`, `internal/config/config.go`
- Related docs: `docs/brainstorms/patch-transfer-system.md`, `docs/plans/2026-05-25-001-feat-file-exchange-system-plan.md`
