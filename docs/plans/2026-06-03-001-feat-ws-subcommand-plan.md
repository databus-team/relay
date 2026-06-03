---
origin: docs/brainstorms/2026-06-03-ws-subcommand-requirements.md
date: 2026-06-03
plan-type: feat
status: active
deepened: 2026-06-03
---

# `relay ws` Subcommand

## Summary

Add a new read-only `ws` subcommand to `relay` that lists the workspace IDs declared in the user's YAML config. Eliminates the need to open the config file just to look up an ID before invoking `pull` / `push` / `list` / `cleanup` / `exec`. Supports three output modes (default ID list, `-v` table, `--json`) and a single-workspace lookup (`--name=<ID>`).

## Problem Frame

`relay pull/push/list/cleanup/exec` all require `-w <ID>`, but the canonical ID list lives in the YAML config. Today the only way to confirm a valid ID is to open the file — slow and error-prone. The new subcommand is a thin read-only introspection helper: load config, iterate `cfg.Watch`, print. No backend connection, no state mutation, no job execution.

Origin: see `docs/brainstorms/2026-06-03-ws-subcommand-requirements.md` for full requirements (R1–R15).

## Requirements

Traced to the origin document. Every R-ID below is sourced from the requirements doc; verify in `docs/brainstorms/2026-06-03-ws-subcommand-requirements.md` for the full text.

| Req | Statement (paraphrased) | Unit |
|-----|-------------------------|------|
| R1, R2, R3 | Command surface: new `ws` subcommand reuses `--config/-c`, read-only | U1 |
| R4 | Default output: one ID per line | U2 |
| R5, R6 | `-v/--verbose` table with `ID / REMOTE_DIR / LOCAL_DIR / JOB_COUNT`; fixed column widths (40 / 30 / 30 / 10) | U2 |
| R7, R8, R9 | `--json` outputs a JSON array of full `WatchConfig` records; mutually orthogonal to `-v` | U2 |
| R10, R11, R12 | `--name=<ID>` filters to a single workspace; nonexistent ID → stderr + exit 1 | U3 |
| R13 | Config-load failure: stderr + exit 1, matching existing convention | U2, U3 |
| R14 | Empty `watch:` list: empty stdout for default, header-only for `-v`, `[]` for `--json`, exit 0 | U2 |
| R15 | Duplicate IDs: render verbatim, do not silently dedupe | U2 |

## Key Technical Decisions

- **Subcommand name = `ws`**: short, no conflict with existing `list` (which lists remote files). Matches the user-confirmed name in the brainstorm.
- **No new dependencies**: `encoding/json` is already imported (`cmd/relay/main.go:5`); the new command reuses it. AGENTS.md (lines 50-53) forbids new frameworks.
- **No backend instantiation**: unlike all 7 existing `runXxx` functions, `runWorkspaces` calls only `config.Load(*configPath)` and iterates `cfg.Watch`. Backend construction is unnecessary for a config-introspection command. This is a deliberate divergence from the existing pattern, called out in `cmd/relay/main.go:research` notes.
- **Reuse `runList` table format** (`cmd/relay/main.go:190-198`): fixed-width columns, `strings.Repeat("-", N)` separator. Decided in plan-time synthesis: fixed widths win over dynamic widths because consistency with `runList` is more valuable than not truncating long `WatchDir` values.
- **JSON output emits the full `WatchConfig` struct**: simpler, future-proof (new fields appear automatically), and consistent with the existing `json.Marshal` use in the exchange protocol (`cmd/relay/main.go:444`).
- **Reuse `config.GetWatchByID` for `--name`**: it already returns the canonical `watch not found: <id>` error. The new command only adds the lookup + error-render, no new error message.
- **Command declaration goes after `syncCmd` (line 58), dispatch case goes after `syncCmd.FullCommand()` (line 84)**: matches the file's bottom-up add pattern.
- **`-v` and `--json` are mutually orthogonal flags**: each governs its own output path. `JSON` mode wins if both are set, since JSON is the more specific contract — but the plan will NOT add an explicit error or warning, treating it as "later flag wins" / "JSON takes precedence" silently. If a future need surfaces, easy to add a warning.

## Scope Boundaries

- **In scope**: new `ws` subcommand with three output modes and a single-workspace filter. One new file: `cmd/relay/main_test.go` (currently absent; required by AGENTS.md line 38 testing convention).
- **Out of scope** (not in this plan, no implied commitment):
  - Shell completion script generation (`make completions` style).
  - TUI / interactive selection.
  - Validation of workspace reachability (would require backend calls — explicitly excluded by R3).
  - Top-level field exposure (`interval_seconds`, `backend.type`) — workspace-scoped only.
  - Config file mutation.
- **Deferred to follow-up work**:
  - Detailed-mode column-width algorithm refinements (current plan uses fixed widths per `runList` precedent; future iteration could go dynamic).
  - Localization of error messages (current plan matches existing English-only convention).
  - `--name` short flag `-n` (not requested in brainstorm; trivial to add later).

## Implementation Units

### U1. Register `ws` subcommand skeleton

- **Goal**: Add the `wsCmd` declaration, optional flags (`--name`, `--json`, `-v/--verbose`), and dispatch case so that `relay ws` and `relay ws --help` work and dispatch into a not-yet-implemented handler.
- **Requirements**: R1, R2, R3.
- **Dependencies**: none.
- **Files**:
  - `cmd/relay/main.go` (modify): add `wsCmd` block after `syncCmd` declaration (~line 58); add `case wsCmd.FullCommand(): runWorkspaces()` in the switch (~line 84).
  - `cmd/relay/main_test.go` (create): new file with the test scaffolding (test fixtures for representative config). Stub-only at this stage; full table-driven cases in U3.
- **Approach**:
  - Declaration block: `wsCmd = kingpin.Command("ws", "List configured workspaces from config")` plus three optional flags mirroring the kingpin style at `cmd/relay/main.go:29, 50`. **Do not** add `.Required()` to any of the three — they are all optional filters.
  - Avoid short flag `h` (taken by the help wiring at `cmd/relay/main.go:62`).
  - Dispatch case: insert after `case syncCmd.FullCommand(): runSync()` (line 84).
  - Stub `runWorkspaces()` body: load config, print one placeholder line. The point of this unit is to lock the wiring before the logic lands.
- **Patterns to follow**: `syncCmd` declaration (line 58, simplest template); `execCmd` flag pattern (line 50, optional `String()`); `debugFlag` pattern (line 29, optional `Bool()`).
- **Test scenarios** (stub verification):
  - `TestWsCommand_Registered`: parse `"ws"` and assert it returns `wsCmd.FullCommand()`.
  - `TestWsCommand_Help`: parse `"ws", "--help"` and assert exit-code / no-panic.
  - `TestWsCommand_NoFlags_AcceptsEmptyConfig`: call `runWorkspaces()` via a thin test wrapper, with a temp config file containing an empty `watch:` list; expect exit 0, no output beyond a placeholder that U2 will replace.
- **Verification**: `go build ./...` succeeds; `./relay ws --help` prints the help text; `./relay ws` with the existing `config.example.relay.yaml` does not panic and emits the placeholder.

### U2. Implement default / verbose / JSON output

- **Goal**: Make `runWorkspaces` produce the three documented output shapes, honoring the `--name` filter when present (defer error rendering for missing IDs to U3).
- **Requirements**: R4, R5, R6, R7, R8, R9, R13, R14, R15.
- **Dependencies**: U1.
- **Files**:
  - `cmd/relay/main.go` (modify): flesh out `runWorkspaces()` body; add a small `printWorkspacesTable(watches []config.WatchConfig)` helper if it improves readability.
- **Approach**:
  - Branch on `*wsJSON` first: marshal the full slice (filtered by `--name` if set) with `json.MarshalIndent` for human readability, falling back to `json.Marshal` if compact is preferred. Decision: use `json.MarshalIndent("", "", "  ")` to match the readability of `-v`. **If both `--json` and `-v` are set, JSON wins silently** (see KTD).
  - Else if `*wsVerbose`: print a header row matching `runList`'s `fmt.Printf("%-40s %10s  %s\n", ...)` style, but for our four columns: `ID` (40-wide left), `REMOTE_DIR` (30-wide left), `LOCAL_DIR` (30-wide left), `JOBS` (right-aligned, width 5). Separator: `strings.Repeat("-", 110)` (or whatever the actual rendered width sums to — implementer measures at runtime).
  - Else (default): `for _, w := range filtered { fmt.Println(w.ID) }`.
  - Filter logic: if `*wsName != ""`, replace `cfg.Watch` slice with `[w]` where `w.ID == *wsName`. U3 owns the "not found" error path; U2's filter is silent on miss (empty output) so U3 can wrap it.
  - Empty `watch:` list: each branch returns cleanly (no rows / header-only / `[]`).
  - Duplicate IDs: do not dedupe; iterate as-is.
- **Patterns to follow**:
  - `runList` table output (`cmd/relay/main.go:185-198`): column widths, separator, per-row `Printf`.
  - `json.Marshal` in `runSync` (`cmd/relay/main.go:444`): already-imported pattern.
  - Config-load error rendering (`cmd/relay/main.go:122-126` for `runPull`): the `fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)` + `os.Exit(1)` shape.
- **Test scenarios**:
  - `TestWsCommand_DefaultIDs` (happy path): config with 3 workspaces → 3 lines, each an ID, no header, in file order.
  - `TestWsCommand_DefaultEmpty`: config with empty `watch:` → no output, exit 0.
  - `TestWsCommand_VerboseTable` (happy path): 3 workspaces → header + separator + 3 rows, each with correct 4-column layout; capture stdout and assert exact bytes (table-driven for fixture configs).
  - `TestWsCommand_VerboseEmpty`: empty list → header + separator only, exit 0.
  - `TestWsCommand_JSON` (happy path): 3 workspaces → valid JSON array of 3 elements, each containing all `WatchConfig` fields; parse back with `json.Unmarshal` and assert deep-equal.
  - `TestWsCommand_JSONEmpty`: empty list → `[]\n` (or just `[]`).
  - `TestWsCommand_DuplicateIDs`: config with two `id: foo` entries → both rendered in default mode (no dedupe).
  - `TestWsCommand_BadConfig`: malformed YAML → stderr contains `"Failed to load config"`, exit code 1.
  - `TestWsCommand_ConfigNotFound`: nonexistent path → stderr contains config-load error, exit 1.
- **Verification**: each test scenario above; `make test` and `make vet` both pass; manual smoke test against `config.example.relay.yaml` (3 workspaces → 3 lines default; `-v` produces the table; `--json` produces valid JSON parseable by `jq`).

### U3. Single-workspace filter and missing-ID error

- **Goal**: `--name=<ID>` returns a single workspace; nonexistent ID emits the canonical error and exits 1.
- **Requirements**: R10, R11, R12.
- **Dependencies**: U2 (U3 layers the lookup + error on top of U2's filter+render plumbing).
- **Files**:
  - `cmd/relay/main.go` (modify): in `runWorkspaces`, after loading config, if `*wsName != ""` call `cfg.GetWatchByID(*wsName)`; on error, `fmt.Fprintf(os.Stderr, "Failed to find workspace: %v\n", err)` + `os.Exit(1)`. On success, build a 1-element slice and hand off to U2's render path.
  - `cmd/relay/main_test.go` (modify): extend the table-driven cases from U2.
- **Approach**:
  - Replace U2's silent filter with an explicit lookup via `config.GetWatchByID` (`internal/config/config.go:108-115`). This guarantees a clear "not found" error message — the existing `fmt.Errorf("watch not found: %s", id)` becomes the canonical text.
  - On hit, set the render slice to `[w]` and continue to U2's branch.
  - Error phrasing: `"Failed to find workspace: <err>"` — diverges slightly from the codebase's two existing idioms ("Failed to ... " and "Noun error: ... ") but is consistent with the former and reads naturally. Alternative considered: `"Workspace not found: %s\n"` with the ID directly. The chosen phrasing preserves the underlying error's casing and keeps the door open to multi-cause errors in the future. **Implementer note**: confirm the phrasing with the README's CLI examples before committing if the project's CLI style guide has firmed up.
- **Patterns to follow**:
  - `runPull` lookup-and-error pattern (`cmd/relay/main.go:128-132`): the `GetWatchByID` + stderr + exit 1 shape, with the same `"Watch error: %v"` text. Our text is slightly different ("Failed to find workspace") but the structure is identical.
  - `config.GetWatchByID` error format (`internal/config/config.go:114`): canonical message lives there.
- **Test scenarios**:
  - `TestWsCommand_NameHit_Default`: `--name=web-app-patches` against the example config → 1 line `web-app-patches`, exit 0.
  - `TestWsCommand_NameHit_Verbose`: same with `-v` → 1-row table.
  - `TestWsCommand_NameHit_JSON`: same with `--json` → single-element JSON array.
  - `TestWsCommand_NameMiss`: `--name=does-not-exist` → stderr contains `"watch not found: does-not-exist"`, exit 1, no stdout.
  - `TestWsCommand_NameEmpty`: `--name=""` (or unset) → behaves as no `--name` (full list).
  - `TestWsCommand_NameWithDuplicateID`: config with two `id: foo`; `--name=foo` → still 1-element (because `GetWatchByID` returns the first match — this is acceptable per the existing function's contract; document in test name).
- **Verification**: scenarios above; `make test` passes; manual smoke: `relay ws --name=web-app-patches` returns the single workspace; `relay ws --name=nope` returns the canonical error.

## Risks & Dependencies

- **No new dependencies**: explicitly confirmed against AGENTS.md (lines 50-53). `encoding/json` is already in the import block (`cmd/relay/main.go:5`).
- **No backend changes**: the new command is read-only against the config; it does not touch `internal/backend` and does not affect the `watch` daemon. The recent `processed map` fix in `a482a79` and the staged-config-reload semantics in `3a539d2` are unrelated to this plan.
- **Behavior risk: `--json` + `-v` combined**: silent JSON precedence is the agreed contract, but if a future user expects `-v` to "always" apply, the silent override could surprise. Mitigation: document in `--help` text (e.g., "When --json is set, output is JSON regardless of -v"). Implementation note: keep the help string short and factual; avoid ambiguity.
- **Behavior risk: fixed-width truncation**: `WatchDir` longer than 30 chars will be truncated in `-v` mode. Acceptable trade-off for consistency with `runList`; document in plan but not in help text (help text should describe the flag, not its limitations).
- **Risk: missing test file baseline**: `cmd/relay/main_test.go` does not exist as of writing. Creating it for the first time is a one-time tax; subsequent commands will have a template. Not a risk for the plan, but worth noting.

## Acceptance Examples

The following mirror the brainstorm's R-IDs and trace directly to test scenarios.

- **AE1** (covers R4): `relay ws` with the example config prints 3 lines, one ID each, in file order. → U2 / `TestWsCommand_DefaultIDs`.
- **AE2** (covers R5, R6): `relay ws -v` with the example config prints the 4-column table. → U2 / `TestWsCommand_VerboseTable`.
- **AE3** (covers R7, R8, R9): `relay ws --json` output is valid JSON; `| jq '.[0].id'` returns the first ID. → U2 / `TestWsCommand_JSON`.
- **AE4** (covers R10, R11): `relay ws --name=missing` exits 1 with "watch not found: missing" on stderr. → U3 / `TestWsCommand_NameMiss`.
- **AE5** (covers R14): `relay ws` against a config with empty `watch:` list exits 0 and produces no stdout. → U2 / `TestWsCommand_DefaultEmpty`.

## Sources & Research

- **Origin document**: `docs/brainstorms/2026-06-03-ws-subcommand-requirements.md` (R1–R15, AE-style acceptance examples not present in the brainstorm; AEs above are plan-local and trace to the brainstorm's success criteria).
- **Local research**: `cmd-relay-patterns` subagent run on 2026-06-03 (output retained in session; not persisted to disk). Key findings: `runList` table format at `cmd/relay/main.go:190-198`; `WatchConfig` field set at `internal/config/config.go:22-28`; `GetWatchByID` at `internal/config/config.go:108-115`; error-rendering idioms catalogued across all `runXxx` functions.
- **External research**: skipped per Phase 1.2 — local pattern density (7 `runXxx` templates, 1 established table format) exceeds the "thin local grounding" threshold, and kingpin is already in use with no version-specific risk in this scope.
- **AGENTS.md constraints**: lines 32 (one `runXxx` per subcommand), 38 (`_test.go` beside source, table-driven), 42 (stderr + `os.Exit(1)`), 50-53 (no new dependencies or frameworks).
