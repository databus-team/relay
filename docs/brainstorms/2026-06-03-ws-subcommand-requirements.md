---
date: 2026-06-03
topic: ws-subcommand
---

# `ws` Subcommand for Listing Configured Workspaces

## Problem Frame

使用 `relay pull/push/list/cleanup/exec` 时必须通过 `--watch/-w` 指定工作区 ID，但 ID 列表只存在于 YAML 配置文件里。当前唯一确认方式是用编辑器打开配置文件，在多个工作区之间定位 ID 既慢又容易拼错。需要在 CLI 内提供一个轻量的"看一眼"入口。

## Requirements

**Command Surface**
- R1. 新增 `relay ws` 子命令，与现有 `watch/pull/push/list/cleanup/exec/sync` 处于同一命令命名空间。
- R2. 复用 `--config/-c` 标志读取配置文件，不引入新的配置加载路径。
- R3. 命令只读配置文件，不连接后端、不执行任何 job、不修改任何状态。

**Default Output**
- R4. 默认输出为每行一个工作区 ID 的纯文本列表，便于管道和 grep 使用。

**Verbose Mode**
- R5. `-v/--verbose` 切换为对齐表格，至少包含 `ID`、`REMOTE_DIR`、`LOCAL_DIR`、`JOB_COUNT`（jobs 数量）四列。
- R6. 详细模式下的列宽随内容动态调整，最小宽度为列标题宽度。

**JSON Output**
- R7. `--json` 标志输出机器可读的 JSON 数组，每条记录至少包含 `id`、`watch_dir`、`local_dir`、`paths`、`jobs` 五个字段（与 `WatchConfig` 结构对齐）。
- R8. `--json` 与 `-v` 互不冲突；JSON 模式仅输出 JSON 本身，不打印表格。
- R9. `--json` 适用于脚本调用、agent 工具集成等场景。

**Single Workspace Lookup**
- R10. `--name=<ID>` 限定为单个工作区；与 `-v` 组合时仅显示该工作区那一行表格，与 `--json` 组合时输出单元素数组。
- R11. `--name` 指定的 ID 不存在时，向 stderr 输出明确错误信息并以非零退出码退出。
- R12. 不指定 `--name` 时，遍历配置中所有 `watch` 条目。

**Error Handling**
- R13. 配置文件加载失败（路径无效、YAML 解析错误等）按现有惯例 `fmt.Fprintf(os.Stderr, ...) + os.Exit(1)`，与 `pull/push/runWatch` 等保持一致。
- R14. 配置中 `watch` 列表为空时，默认输出为空、`-v` 仅打印表头、`--json` 输出 `[]`，全部以退出码 0 退出（这是合法配置，不是错误）。
- R15. 配置中两个工作区 ID 相同（重复）时不静默去重；按配置文件原样输出，让用户看到问题。

## Success Criteria

- `relay ws` 输出所有工作区 ID，顺序与配置文件一致。
- `relay ws -v` 输出对齐的表格，包含 `ID / REMOTE_DIR / LOCAL_DIR / JOB_COUNT`。
- `relay ws --json` 输出合法 JSON 数组，可被 `jq` 等工具直接消费。
- `relay ws --name=<ID>` 在 ID 存在时仅输出该工作区信息；ID 不存在时返回非零退出码与明确错误。
- 各模式之间不产生额外的进度信息或空行污染输出。
- 命令不触发任何后端连接、不执行 job、不修改文件系统。

## Scope Boundaries

- 不修改 `config.yaml` 格式，不引入新的配置字段。
- 不校验工作区可达性、不 ping 远端、不列举远端文件。
- 不做交互式选择（无 TUI）。
- 不提供 shell 补全脚本生成（属于独立的后续工作）。
- 不暴露顶层字段（`interval_seconds`、`backend.type` 等），聚焦工作区本身。

## Key Decisions

- 子命令名定为 `ws`：与 `list` 命名空间无冲突（`list` 已被"列远端文件"占用），且简短易记。
- 默认输出 ID-only 而非表格：脚本友好，详细信息需要时通过 `-v` 获取。
- 复用 `config.Load()`：自动获得环境变量展开、Windows 路径归一化等既有行为。
- 复用 `runList()` 的 `formatSize`-风格列对齐输出惯例（`fmt.Printf("%-Ns ...") + strings.Repeat("-", N)`）。
- `--json` 与 `-v` 互不冲突：JSON 模式只输出 JSON，verbose 模式只输出表格。
- 错误处理沿用现有模式：`stderr + os.Exit(1)`，与 `runPull`/`runPush`/`runWatch` 一致。

## Dependencies / Assumptions

- `internal/config` 包继续作为唯一配置加载入口；`WatchConfig` 结构（`ID`/`WatchDir`/`LocalDir`/`Paths`/`Jobs`）保持稳定。
- kingpin 继续作为 CLI 框架；`ws` 命令遵循现有 `kingpin.Command(...)` 注册模式（与 `syncCmd`/`cleanupCmd` 同形态）。
- 不引入新依赖。
- 用户对工作区 ID 的拼写无外部校验需求（拼写错误会以 `Watch not found: <id>` 形式被既有 `GetWatchByID` 暴露，行为不变）。

## Outstanding Questions

### Deferred to Planning
- 详细模式列宽的精确算法（最大列宽 vs 固定列宽 vs 截断策略）。
- 是否需要为 `--name` 增加简写 `-n`（与现有 kingpin 短标志惯例一致）。
- `-v` 详细模式是否应该把 `paths` 模式列表（`*.patch` 等）也展示出来。
- 错误信息的中英文策略：项目既有中文注释也有英文日志，需在实现时统一。

## Next Steps

-> /ce:plan for structured implementation planning
