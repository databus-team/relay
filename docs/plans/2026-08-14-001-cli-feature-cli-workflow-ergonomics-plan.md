---
title: CLI 工作流顺手化 - Plan
type: feat
date: 2026-08-14
topic: cli-workflow-ergonomics
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-brainstorm
execution: code
---

## Goal Capsule

**Objective** 把 `relay` 的日常推拉拉流程做顺：push/pull/list/exec 未指定 `-w` 时按当前目录推断 workspace；`pull` 可选地在拉取成功后删除远端文件；新增 `relay job run` 在本机手动执行配置中定义的 job。

**Product authority** 单一 CLI 工具（用户单人使用），所有命令基于同一份本地 config。

**Open blockers** 无。

Product Contract 未改动（in-place enrichment，requirements-only → implementation-ready）。

---

## Product Contract

### Summary

把 `relay` 的推拉拉流程做成少敲参数的风格：`-w` 不传时用当前目录名推断 workspace，推断不出就报错并列可选；`pull` 新增一个可选项，拉取成功后把远端文件一并删掉；新增 `relay job run <jobID> [file]` 子命令，在本机手动执行配置里定义的 job，方便拉完 patch 后直接跑 `git am --3way` 之类的后置命令。

### Problem Frame

目前 `push`/`pull`/`list` 每次都要求显式传 `-w`，而用户的工作区目录名本就与 config 里的 watch id 一一对应，重复输入没有信息量。拉取后平时的收尾常连着一串手工操作：先回后台删远端文件、再在本地跑 `git am`，这些步骤现在要么靠 watcher 的 jobs 自动跑、要么彻底手搓。watcher 已具备一套 per-watch 的 job 定义和变量替换能力，但只随 watcher 进程运行，无法按需手动触发。三个痛点是同一条"拉 patch → 应用 → 清理"流程上的割裂点。

### Requirements

**Workspace 推断（-w 默认值）**

- R1. `push`、`pull`、`list`、`exec` 的 `-w` 从必填改为可选；省略时按当前工作目录的 basename 匹配 config 中的 watch `id` 推断 workspace。
- R2. 当 basename 匹配到 0 个或多个 watch 时，报错并列出可用 workspace（id + local dir）。显式传 `-w` 的优先级高于推断。
- R3. 显式指定 `-w` 时的现有行为不变。

#### pull 拉取后删除远端

- R4. `pull` 新增可选项（如 `--delete`），在本地文件拉取成功之后删除同一个远端文件。
- R5. 删除是独立的收尾步骤：本地拉取失败时不触碰远端；远端删除失败单独报错，不掩盖本地拉取的成功。

#### 手动 job 执行

- R6. 新增 `relay job run <jobID> [file]` 子命令，在本机执行 config 中该 workspace 下指定的 job；workspace 按 R1 的推断规则解析。
- R7. exec 类型的 job 在本地（默认以 local_dir 为 cwd）执行，`file` 入参绑定到 `{file_path}` 等变量；沿用 watcher 现有的变量替换集合（`file_path`、`file_name`、`file_dir`、`file_remote_path`、`timestamp`）。
- R8. 若 job 执行依赖某个文件（引用了文件类变量）但未提供 `file`，报错并提示需要文件名。
- R9. job 执行失败时返回非零退出码，并输出命令的 stdout/stderr。

### Key Flows

- F1. **拉后删**
  - **Trigger:** 用户运行 `relay pull -d <file>`
  - **Steps:** 解析 workspace → 读取远端文件 → 写入本地文件 → 删除远端文件
  - **Outcome:** 本地落盘；远端文件已删；任一步失败均有明确报错。
- F2. **手动 apply**
  - **Trigger:** 用户在 workspace 目录运行 `relay job run apply <file>`
  - **Steps:** 按 cwd 推断 workspace → 在 config 中定位 `apply` job → 把 `<file>` 绑定到 `{file_path}` → 本地执行 `git am --3way {file_path}`
  - **Outcome:** 本地完成 git 应用；失败退出码非零并输出错误。

### Acceptance Examples

- AE1. 在 `/d/Group_Projects/databus_backend` 下运行 `relay pull a.patch` → 推断 workspace 为 `databus_backend`，拉取 `a.patch` 到当前目录。
- AE2. 在无关目录运行 `relay pull a.patch` → 推断失败，报错并列出现有 workspace。
- AE3. `relay pull -d a.patch` → 本地落盘且远端 `a.patch` 被删除。
- AE4. 远端读取失败时运行 `relay pull -d` → 本地无文件、远端文件保留。
- AE5. `relay job run apply /d/Group_Projects/databus_backend/a.patch` → 在本机执行 shell 命令，`{file_path}` 替换为传入路径。
- AE6. 对一个依赖文件的 job 运行 `relay job run apply`（未给文件）→ 报错提示需要文件。

### Key Decisions

- **本地选型（session-settled: user-directed — chosen over 远端执行）**——手动 job 在本地执行，因为拉下来的 patch 就在本机，目标是做一条本地后置命令的捷径。
- **独立 subcommand 而非 pull 选项（session-settled: user-directed — chosen over `pull --run`）**——`relay job run` 是通用机制，点与拉取解耦，可脚本化。Governs R6。
- **`-w` 推断统一到 `exec`（session-settled: user-approved — chosen over 保持 exec 原样）**——避免 `exec` 与其它命令两套规则。Governs R1。
- **手动触发忽略 job 的 `if` 条件**——`if` 依赖前序 job 的结果，单次手动 run 没有该上下文，故跳过。Governs R8。

### Scope Boundaries

**积极排除（本计划不做）**

- 手动 job 的远端执行——既有 watcher/exec 转发机制已存在，不新增远端手动触发。
- 在手动路径下不依赖、不修改 watcher 的运行状态或 `processed` 去重数据。
- 不新增自动重试/通知机制。

### Dependencies / Assumptions

- 复用 config 中 per-watch `jobs`（exec / file_delete 类型）与 watcher 现有的变量替换逻辑。
- 依赖后端提供的 `Delete` 能力（已在 `runCleanup`/`runSync` 中调用过）。
- 假设当前目录 basename 与 watch id 的对应关系成立（用户 config 中已如此组织）。

### Sources / Research

- `cmd/relay/main.go` —— push/pull/list/exec 的 `-w` 均为 `.Required()`，`runPull` 只有读取+本地写入、无删除。
- `internal/config/config.go` —— `WatchConfig`/`JobConfig` 及字段。
- `internal/watcher/watcher.go` —— `executeJobs` 的 exec/file_delete 分支、`buildVariables`/`substituteVariables` 的 `{变量}` 替换。
- `internal/backend/backend.go` / `internal/backend/local.go` —— `Delete` 接口签名与 local 后端实现（可用于测试队形）。

---

## Planning Contract

### Key Technical Decisions

- KTD1. **新增独立包负责手动 job 执行（session-settled: user-directed — chosen over 直接在 main 里展开；挂在 `本地选型` 决策下）**——在 `internal/jobrunner` 中实现本地执行逻辑，同时天然可单元测试。`internal/jobrunner` 自构变量表（`file_path` 绑定本地入参），仅复用 `substituteVariables`（本就是包级函数，导出为 `SubstituteVariables` 即可）；`buildVariables` 保持为 watcher 的 `*Watcher` 方法不动，避免无意义的跨包方法提升。Governs R6-R9。
- KTD2. **命令形态 `relay job <run|…>`（session-settled: user-directed — chosen over `pull --run`）**——用 kingpin 嵌套子命令注册 `relay job run <jobID> [file]`，`-w` 可选，落定早前的 `relay job run` 形态。Governs R6。
- KTD3. **共享 `resolveWorkspaceID` 推断 -w（session-settled: user-approved — chosen over 按 local_dir 匹配）**——在同一包提供 `resolveWorkspaceID(cfg, provided)`：显式值优先，否则按 cwd basename 匹配 watch id；0 或多个命中时也报错并列出可选。`exec` 采用宽松回退（解析不到就维持无工作区执行，不破坏 `exec` 的原生转发）。Governs R1-R3。
- KTD4. **手动 run 对 file_delete 落地为本地删除、按 `job.Timeout` 限时**——沿用 watcher 的 `exec/file_delete` 分支语义，只是把 `b.Delete` 换成 `os.Remove`、把 `b.Exec` 换成本地 `sh -c`。覆盖 R7-R9。
- KTD5. **`file` 本地文件的变量绑定**——传入 `file` 时：`file_path`/`file_remote_path`=绝对路径、`file_name`=basename、`file_dir`=目录；未传时这几个文件变量为空。判断 job 是否“需要文件”以 job 命令/路径中是否出现 `{file_*}` 记号为准。Governs R7-R8。

---

## Implementation Units

### U1. workspace 推断 helper + push/pull/list 的 `-w` 改为可选

**Goal** 让 push/pull/list 省略 `-w` 时按 cwd 推断 workspace，命中不了就报错列可选。

**Requirements** R1, R2, R3

**Dependencies** 无

**Files**
- 修改 `cmd/relay/main.go`
- 修改 `cmd/relay/main_test.go`

**Approach**
1. 去掉 push/pull/list 的 `-w` 的 `.Required()`。
2. 新增私有函数 `resolveWorkspaceID(cfg *config.Config, provided string) (string, error)`：
   - `provided != ""` → 直接返回。
   - 否则取 `os.Getwd()`，以 `filepath.Base(cwd)` 在 `cfg.Watch` 扫描 `w.ID == base`。
   - 恰一个命中 → 返回该 id；0 个或 ≥2 个 → 构造报错（含当前目录、以及从 `cfg.Watch` 收集的 workspace 列表 id + local_dir）。
3. 在 `runPush`/`runPull`/`runList` 中用调用结果替换 `*pushWatch`/`*pullWatch`/`*listWatch`，解析失败 `os.Exit(1)`。

**Patterns to follow** `cmd/relay/main.go` 里 `resolveWatches`（同一职责的已有 resolver，错误信息风格一致）；`main_test.go` 的 `withTempConfig` / `withCapturedStdout` 测试工具。

**Test scenarios**
- `resolveWorkspaceID(cfg,"")`，cwd 即某 watch id 的临时目录（用 `t.Chdir`）→ 命中返回该 id。覆盖 AE1。
- `resolveWorkspaceID(cfg,"watch")` 显式值 → 忽略 cwd 直接返回该值。
- cwd 名在 config 中不存在 → 返回错误且错误包含 workspace 列表。覆盖 AE2。
- cwd 名命中 2 个 watch id → 返回错误（歧义），错误含 id + local_dir。
- 列表示空 watch：未提供 `-w` 且 cwd basename 无命中 → 报错但列表为空，不 panic。

### U2. exec 的 `-w` 推断（宽松回退）

**Goal** 让 `exec` 省略 `-w` 时也按 cwd 试推断，供 cwd/健康检查使用，但不因推断失败而破坏 exec 现有的无工作区转发。

**Requirements** R1, R3

**Dependencies** U1

**Files**
- 修改 `cmd/relay/main.go`
- 修改 `cmd/relay/main_test.go`

**Approach**
1. `runExec` 分支开头：`w := *execWatch`；`w == ""` 时调用 `resolveWorkspaceID(cfg,"")`，命中 → 用该 workspace 继续（cwd、健康检查），未命中 → 保持现行为（转发 + 打印已有 "Note: …" 提示）。

**Test scenarios**
- 在匹配 workspace 的 cwd 下以 local（non-exec）后端跑 `exec`：输出不应出现旧的无工作区提示态文案（证明推断命中了工作区）。U1 已完成 resolver 本身的断言。
- 未匹配 cwd + 未传 `-w` 跑 `exec`：回退到无工作区转发路径（打印已有提示，不报“workspace not found”错误）。

### U3. pull 拉取后删除远端（`--delete`）

**Goal** 拉取成功后按需删除同一远端文件。

**Requirements** R4, R5

**Dependencies** 无

**Files**
- 修改 `cmd/relay/main.go`
- 修改 `cmd/relay/main_test.go`

**Approach**
1. `pull` 新 flag：`--delete`（短名 `-d`，`Bool`）。
2. `runPull`：本地 `os.WriteFile` 成功且设置了 flag 时调用 `b.Delete(ctx, remotePath)`。若 `Delete` 失败，向 stderr 打印删除警告，但整体仍按"拉取成功"结束（保留 0 退出码）。Remote read fail → 直接报错，不执行删除。

**Test scenarios**
- 配置 local 后端，临时 base_dir，预置远端文件；设 `-d` 且 `t.Chdir` 到临时目录跑 `runPull`：本地落盘且远端文件消失。覆盖 AE3 / AE4（远端不存在时 read 报错、远端文件保留）。
- 未设 `-d` 跑 `runPull`：远端文件保留。
- `--delete` 但 `b.Delete` 报错（用一个返回错误的 stub backend 注入）→ 仍打印成功落地，仅删除侧出警告，退出码为 0。

### U4. 手动 job 执行：`relay job run <jobID> [file]`

**Goal** 本机手动执行 config 中定义的 exec / file_delete job。

**Requirements** R6, R7, R8, R9
**Dependencies** U1（取其 resolver 与 `-w` 推断）。本单元不消费 U3：`file_delete` 走本地 `os.Remove`，后端 `Delete` 能力已在既有 `runCleanup`/`runSync` 使用，不依赖 U3 的改动。

**Files**
- 新增 `internal/jobrunner/jobrunner.go`（执行核心）
- 新增 `internal/jobrunner/jobrunner_test.go`
- 修改 `cmd/relay/main.go`（命令注册 + 调度 + 退出码）
- 修改 `internal/watcher/watcher.go`（将包级 `substituteVariables` 导出为 `SubstituteVariables` 供 jobrunner 复用；`buildVariables` 不动）

**Approach**
1. kingpin 注册嵌套子命令 `job run`：`-w` 可选、参数 `jobID` 必填、`file` 可选。`main` 里新增 `runJobRun()`。
2. `runJobRun`：加载 config → `resolveWorkspaceID`（这里如同 R2 报错，不做宽松回退，因 job 必须落在某个 config）→ 在 `cfg.Watch` 中找 jobID；找不到回报错。
3. 委托 `internal/jobrunner.JobRun(cfg watchConfig, jobID, file)`：
   - 解析 `job`（exec / file_delete），判断是否引用 `{file_*}` 变量且 `file` 未提供 → 返回错误（R8）。
   - 组装变量：`file_path`/`file_remote_path`=abs(file)、`file_name`=basename、`file_dir`=目录、`timestamp`=RFC3339。
   - exec：`substitute(cmd)`，`cwd`=job.Cwd 缺省 `local_dir`；本地 `sh -c`（带 job.Timeout 的超时）执行，返回 stdout/stderr；非 0 退出 → 错误。
   - file_delete：`substitute(job.Path)`→ 本地 `os.Remove`。
   - 手动 run 忽略 job 的 `if`（无前序上下文）。
4. `main` 打印执行结果并据此 `os.Exit(非0/0)`（R9）；stderr 承载报错。

**Technical design（示向，非实现规范）**

```text
resolveWorkspaceID → cfg.Watch[id].Jobs:[jobID]
 ↓
if jobCmd/path references "{file_*}" && file == "" → error (R8)
 ↓
— exec:      cwd = job.Cwd || watch.LocalDir ; sh -c substitute(job.Cmd) (timeout job.Timeout) → stdout/stderr/exit
— file_delete: os.Remove(substitute(job.Path))
if (exec exit != 0): error → os.Exit(1)
```

**Patterns** 照 `internal/watcher/watcher.go` 的 `runLocalCommand`/`runLocalCommandCapture`（本地执行 + 超时 + 捕获输出）；`substituteVariables`/`buildVariables` 的变量集合同源。

**Test scenarios**
- `internal/jobrunner` 单测（用临时目录 + 构造 watchConfig）：
  - exec job 正确绑定 `{file_path}`（echo/输出含传入绝对路径）。覆盖 AE5。
  - exec job 运行失败 → 返回错误（覆盖 R9）。
  - exec job 缺 `file` 但引用 `{file}` → 返回“需要文件”错误（覆盖 AE6）。
  - 不引用文件的 exec job 未给 file 也可运行（如 `git status`）。
  - file_delete job 删除传入本地文件（受 `path` 变量替换影响）。
  - 默认 cwd 用 `local_dir`：job 在 `local_dir` 下生效。
  - 忽略 `if`：job 带 `if: jobs.xxx.success` 条件，手动直接命中该条件不应被阻塞。
- `cmd/relay` 整单测：`relay job run <jobID>` 非零退出码的调度正确。

**Execution note**：job 执行是纯本地副作用，优先为 `internal/jobrunner` 写表驱动单测锁死变量替换与退出码契约，再接线到 main。

### U5. 文档与示例更新

**Goal** 新行为写进 README 与配置示例。

**Dependencies** U1-U4

**Files**: 修改 `docs/README.md`；修改 `docs/config.example.relay.yaml`（在 jobs 注释位展示 `relay job run` 用途）。

**Approach** 简洁补充：
- `-w` 可省略/按 cwd 推断。
- `pull --delete` 用法。
- `relay job run <jobID> [file]` 一段带说明的子命令用法（含和 `pull` 串联示例）。

**Test expectation**: 无 —— 纯文档变更。

---

## Verification Contract

| 级别 | 命令 | 判定 |
|---|---|---|
| 单元 | `go test ./...` | 全绿，含新增的 resolver / jobrunner / pull --delete 用例 |
| 构建 | `go build ./...` | 成功，无类型/调度错误 |
| 静态 | `go vet ./...` | 无告警 |
| 手动 | `relay ws && (在 workspace 内) relay pull a.patch; relay pull -d a.patch; relay job run apply a.patch` | 各命令按 README 行为工作 |

## Definition of Done

- 未传 `-w` 时 push/pull/list 依据 cwd 推断 workspace；命中不了时报错并列出可选；显式 `-w` 优先。`exec` 宽松推断生效。
- `pull --delete`/`-d` 在本地成功写盘后删除远端；删除失败不掩盖拉取成功。
- `relay job run <jobID> [file]` 在本机执行 job，`git am --3way` 型后置命令可用；依赖文件却不给文件、job 失败均返回非零退出码。
- `go test ./...` 全绿，README/config 示例已更新。