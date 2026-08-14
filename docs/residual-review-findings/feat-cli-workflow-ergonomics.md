# Residual Review Findings

Source: `ce-code-review mode:agent` run `20260814-131252` on branch `feat/cli-workflow-ergonomics` (plan `docs/plans/2026-08-14-001-cli-feature-cli-workflow-ergonomics-plan.md`). Changes shipped in commits up to and including `refactor(cli) + fix(review)`. Review findings below are not applied — they need a product/design decision or are advisory.

## Unapplied actionable / design findings

- **[P2] exec cwd inference now forces a health-check Ping in a matched cwd.** `cmd/relay/main.go` — when `-w` is inferred from the cwd, `relay exec` runs `b.Ping`, which errors if no watcher is running; the pre-change no-workspace path forwarded directly. Fix candidate: Ping only when `-w` was explicitly provided. Needs a decision on exec's intended behavior.
- **[P2] `pull --delete` keeps exit 0 when the remote delete fails.** Scripted consumers can't tell "deleted" from "still present", risking duplicate processing. Fix candidate: a distinct exit code (e.g. 2) for the delete-failure branch. Reframes the plan's R5 "delete failure doesn't mask pull success" — user must choose.
- **[P2] job run exec cwd defaults to config `LocalDir`, not the invocation cwd.** When the cwd basename matched the watch id, `jobrunner.Run` runs with `cwd = watchCfg.LocalDir`, which can be a different (or absent) tree than the user invoked from. Advisory.
- **[P2] `{file_remote_path}` binds to the local path on manual runs** while the watcher binds it to the remote path — the shipped example `cleanup` file_delete job deletes a LOCAL file under `relay job run`. `(session-settled)` KTD5 chose this local binding; kept but flagged as a caller-divergent side effect. Not applied because the user already settled the binding.
- **[P3] `jobrunner.Run(ctx, ...)` never honors ctx.** The captured local runner builds its own fresh context. Advisory; thread ctx or drop the parameter.
- **[P3] relative `file` binds against process cwd, not the exec cwd.** `filepath.Abs(file)` resolves against the invocation dir while the job runs in `LocalDir` — `{file_path}`/`{file_dir}` can point at a different tree for relative args. Advisory.

## Deferred maintainability / coverage

- Maintainability advisory: `fileVarNames` (jobrunner) vs `watcher.buildVariables` keys still list the same file-variable set in two places. Defer the shared-source extraction.
- Maintainability advisory: `watcher.RunLocalCommandCapture` was exported for one consumer (jobrunner); a shared `internal/executil` package is an alternative keep instead.
- Coverage gaps noted by reviewers (not regressions): exec inference-fallback integration test, `pull --delete` delete-failure-branch test (needs an injection seam), `runJobRun` exit-code test, `job.If`-ignored contract test.
- Pre-existing (unchanged-by-this-branch): top-level `--config` and server `--config` collide ("duplicate long flag --config"), which makes the CLI inoperable at parse time on the base tree. Not introduced here.

## Not evaluated

No plan was auto-discovered (`plan:` explicit), so settlement suppression beyond the `file_remote_path` KTD5 case was not evaluated.