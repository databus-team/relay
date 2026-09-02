---
title: 多 executor 工作区可选 并发出网隧道 - Plan
type: feat
date: 2026-09-02
topic: multi-executor-tunnel
execution: code
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-brainstorm
depends_on: docs/plans/2026-09-02-1212-feat-executor-socks5-proxy-plan.md
---

# 多 executor 工作区可选并发出网隧道 - Plan

## Goal Capsule

- **Objective:** 在既有 SOCKS5 出网隧道方案（`2026-09-02-1212-feat-executor-socks5-proxy-plan.md`，本计划之基座）之上，补齐「一台中转同时挂载**两台（或多台）远端 machines 各自的 executor**」的完整闭环：本地可**同时**拉起多条 SOCKS5 隧道，**每条各自绑定一个本地端口、各自选定一个工作区（watch）作为出口 executor**，互不干扰地访问各自 executor 内网可达的目标。核心交付是三点：多 executor 注册基线、`relay tunnel --watch` 工作区选择器、并发多隧道隔离。
- **Product authority:** 本计划**不新增**协议消息、不改白名单/鉴权/出口语义——它把 `2026-09-02-1212` 里「单条隧道、单一 executor」的机制，凡能并排放到多 executor 上即可；`MsgTunnel*` 协议族、executor `network_allow`、executor 本地建连、中转只转发不落地等既有决策全部沿用，本计划只在其上补「多份并存 + 选中哪个出口」的编排。
- **Open blockers:** 无。三个方向决策已在对话中确认：本机入口为手动现起（每隧道一进程）/ 并发多隧道 / 工作区显式可选（缺省回退当前目录名）。

---

## Product Contract

### Summary

两台远端机器 M1、M2 各跑一个 `relay watch`（各持有自己的 watch/工作区），向同一中转注册为 executor。本地用户可同时运行多个 `relay tunnel`：`relay tunnel --listen 127.0.0.1:1080 --watch site-a` 与 `--listen 127.0.0.1:1081 --watch site-b`，两条隧道并行，分别经 M1、M2 出网访问各自内网白名单目标。中转按 watch 路由每条 `MsgTunnelConnect` 到对应 executor（多 executor 注册基线已由 `executors map[watchID]clientID` 天然支持），且每条隧道独立、互不串扰。

### 前置：依赖既有 SOCKS5 方案

本特性**建立在 `2026-09-02-1212`（SOCKS5 隧道）之上**，不复刻其协议/出口/白名单设计。引用其既有资产（如无已实现则视为其同号项）：

| 既有资产（SOCKS5 方案） | 本计划复用 |
|---|---|
| `MsgTunnelConnect/Data/End`（KTD1） | 原样，不改 |
| `relay tunnel --listen` 本地 SOCKS5 伺服（KTD5/U4） | 加 `--watch` 选择器（U1/U2） |
| 中转按 watch 路由执行者（KTD2 校验 `非本 watch` 执行者 fail-fast） | 天然支持多 watch；补并发路由（U3） |
| 中转双向泵注册表（KTD8） | 本已按 tunnelID 独立，天然多隧道；补并发断言测试 |
| executor `network_allow` 白名单 + 本地 `net.Dial`（KTD3/U3） | 原样，多 executor 各自独立策略 |

---

### Requirements

**多 executor 注册基线**

- R1. 同一中转支持**两台（及多台）远端 executor 同时在线注册**，各属于自己的 watch，互不覆盖、互不串扰，并可被 `exec`/`push`/`version`/`status` 独立寻址。（`executors map[string]string` 按 watch 键控已天然满足；本计划把这一点从「隐式成立」提升为「显式承诺」，并补集成测试。）
- R2. 各 executor 的 watch（工作区）可不同名、不同目录；`relay status` 及 `relay version -r` 台账按 watch 清晰展示每个在线 executor 及其构建版本，用户可据此得知有哪些出口可选。

**隧道出口选择**

- R3. `relay tunnel` 接受 `--watch <工作区>` 选择器，语义与 `exec`/`push`/`sync` 完全一致：命中该 watch 在线 executor 的网络出口；**缺省时按当前目录名推断**（复用 `resolveWorkspaceID`，与其他命令一致）。
- R4. 选择的 watch 若 `transit` 上没有注册 executor（未注册 / 掉线），隧道 `CONNECT` 阶段即失败并明确报「无可用执行方」，不建立任何内网连接（延续 SOCKS5 方案的 fail-fast 语义）。

**并发多隧道**

- R5. 多个 `relay tunnel` 进程可同时运行，各自不同本地端口、各自不同 `--watch` 出口，经同一中转并行，双向独立不串扰——一条隧道的抖动/关闭不影响其它。
- R6. 中转隧道注册表（KTD8 按 `tunnelID` 键控）必须并发安全：多隧道建连/拆除/数据帧不出现数据交叉或竞态，无泄漏。

### Key Decisions

- **工作区可选复用既有 `--watch` 寻址体系** `(design — chosen over 新建 executor 名字寻址：与 exec/push/sync 同一套 resolveWorkspaceID，缺省回退当前目录名，CLI 心智连贯，零新增概念)` — Governs R3, R4.
- **交付形态 = 多进程并存（每隧道一进程）** `(user-settled: user-directed — chosen over 单进程多隧道管理器：与现有 CLI「每命令一进程」模型一致（`relay watch`/`relay exec` 本就是独立进程），改动最小；双隧道即双开两进程，不常驻、不用时不占资源；后续如果需要集中管理再加配置化批量)` — Governs R5.
- **并发隔离来自既有 KTD8 隧道注册表** `(design：隧道注册表本就按 tunnelID 独立；本计划不动其结构，只在协议/并发层补「多隧道同 lifetime」的测试断言，不另造复用池/惰性 stub)` — Governs R5, R6.
- **多 executor = 多 watch 一对一** `(design — chosen over 单 watch 广播多 executor：本项目先验证为「多台、多工作区、各自独立」；同一 watch 多 executor 的广播/负载均衡归入 defer，避免把后续语义一并做进来)` — Governs R1.

### Key Flows

**F1. 双隧道并行出网（本地 → 中转 → 两台 executor → 各自内网）**

- **Trigger:** 本地同时运行两个 `relay tunnel`：`--listen 127.0.0.1:1080 --watch site-a` 与 `--listen 127.0.0.1:1081 --watch site-b`（各自绑定合法端口）。
- **Actors:** 本地两个 SOCKS5 客户端 → 本地两实例 `relay tunnel` → 同一中转 transit → executor M1（watch site-a）、M2（watch site-b）。
- **Steps:** C1 连 `127.0.0.1:1080` 发 `CONNECT ina.internal:80`；C2 连 `127.0.0.1:1081` 发 `CONNECT b-intra:443`。Tun-a 把 watch=site-a 的 `MsgTunnelConnect` 发往 M1、Tun-b 发 watch=site-b 给 M2；各自白名单校验通过后分别 `net.Dial` 至各自目标，双向字节流经各自 tunnelID 的注册表往返。三者并行、互不阻塞。
- **Outcome:** 本地两程序像直连内网一样分别访问 M1、M2 的内网白名单目标；各自断连只关各自隧道，另一条不受影响。

**F2 — 失败语义（无可用执行方）**

- Trigger 用户对一台掉线/未注册的机器起隧道：`relay tunnel --listen 127.0.0.2:1082 --watch site-c`；
- Steps 任一 CONNECT 的 watch=site-c 中转上无 executor → `CONNECT` 握手即宣告失败（SOCKS5 回 General Failure）；
- Outcome 本地不建立内网连接、命令打印「无在线执行方 for site-c」，另一条正常隧道不受影响。

### Acceptance Examples

- AE1. **双机双隧道并行** — 两台 executor（site-a/site-b）在线，起两条 `relay tunnel` 各选一 watch；本地两 `curl --socks5-hostname` 分别访问各自联通的目标，两端正向字节流各自就绪、互不串扰；关掉一条，另一条仍可用。Covers R1, R5.
- AE2. **缺省工作区推断** — 在名为 `site-a` 的目录下 `relay tunnel --listen 127.0.0.1:1080`（不传 `--watch`），等价于 `--watch site-a`；与其它命令一致。Covers R3.
- AE3. **无执行方失败** — `--watch site-c`（掉线）时任一 CONNECT 即 FAIL，不建立内网连接、报「无在线执行方」。Covers R4.
- AE4. **台账可分辨** — `relay status` / `relay version -r` 同时展示 site-a、site-b 两个 executor 且各自版本，用户能据此分辨出口。Covers R2.

### Scope Boundaries

- 不做**单 watch 广播 / 负载均衡 / failover**（一台机器故障不自动切到另一台）——保持一对一语义。
- 不做**单进程集中多隧道管理器**（见 Key Decisions）——多隧道=多进程；集中守护留 deferred。
- 不改 `MsgTunnel*` 协议、不改白名单/鉴权/出口语义（出口仍只在 executor 本地建连）。
- 不新增 executor 命名空间——工作区即 watch；不引入不同于 `--watch` 的「executor 名」。

### Success Criteria

- SC-1. 一台中转、两台 executor（site-a/site-b）在线时，本地两条 `relay tunnel`（不同端口/不同 watch）同时可用，分别访问各自内网放行目标成功（R1, R5）。
- SC-2. 缺省 `--watch` 按当前目录名推断；显式 `--watch` 与 `exec/push/sync` 一致（R3）。
- SC-3. 对无执行方的 watch 起隧道，CONNECT 即失败、不建内网连接、报无在线执行方（R4）。
- SC-4. `go test` 全绿；并发多隧道集成用例通过；既有 SOCKS5 单隧道用例不回归。

### Dependencies / Assumptions

- **依赖**：`2026-09-02-1212`（SOCKS5 隧道）已实现——`relay tunnel` 基础、`MsgTunnel*`、中转 KTD8 隧道注册表、executor `network_allow` + `net.Dial`。本计划只在其上演多 executor 编排。
- **假设**：两台远端机器各持有独立的 watch_id 与独立 `network_allow`，各自能出网连接内网目标；中转单实例单进程。
- **假设**：本地能同时持多个到同一中转的 WS 连接（每条 `relay tunnel` 一个长连接），无需共享/复用——与既有 CLI 进程模型一致。

### Outstanding Questions

- **Deferred**：单 watch 多 executor 广播/负载均衡（不定位本 MR）；集中式多隧道守护（`relay tunnel --config`），观察多进程并发后的运维痛点再定；多隧道内网目标冲突/合并提示。— 均不阻塞本 MR。

---

## Planning Contract

### Key Technical Decisions

- **KTD1. `relay tunnel` 增加 `--watch` 选择器，复用 `resolveWorkspaceID`**：新 flag `tunnelWatch = tunnelCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w')`；隧道握手前调 `resolveWorkspaceID(cfg, *tunnelWatch)` 解析出 watch_id，填入 `MsgTunnelConnect` 头部。若目标无在线 executor 则失败（共生 SOCKS5 的 fail-fast）。— 立即 R3, R4.
- **KTD2. `MsgTunnelConnect` 明确携带 `watch_id` 字段**：既有 SOCKS5 `TunnelConnectRequest` 头部补 `watch` 字段（按 watch 路由）；中转 `handlerTunnelConnect` 用 `GetExecutor(watch)` 取出口，无则 fail-fast（延续 `forwardToExecutor`）的 watch 路由语义）。— 立即 R3, R4.
- **KTD3. 中转隧道路由并入现有 `executors`/`executorMu` 一致治理**：隧道由 watch 定向到 executor（`GetExecutor(watchID)`）；并发多隧道由既有 KTD8 隧道注册表（`tunnelID → {reqConn, execConn}`）天然隔离，不重写；对既有互斥锁范式（`executorMu` 等）保持同构，确保多隧道同时建/拆安全。— 立即 R6.

### High-Level Technical Design

本地同时跑多个 `relay tunnel`（各自 `--watch X`、各自 `--listen 端口`）。每个隧道的 `MsgTunnelConnect` 头部带其选取的 watch；中转按该 watch 取已注册 executor 并转发，复用 KTD8 隧道注册表维持双向字节流（按 tunnelID 隔离）。两家 executor 各自校验自己的 `network_allow` 并本地建连。由此多隧道天然互不串扰。

```mermaid
sequenceDiagram
  participant C1 as "本地SOCKS客户端A"
  participant C2 as "本地SOCKS客户端B"
  participant L1 as "tunnel(1080,site-a)"
  participant L2 as "tunnel(1081,site-b)"
  participant T as "中转 transity"
  participant E1 as "executor site-a"
  participant E2 as "executor site-b"
  C1->>L1: CONNECT a:80
  C2->>L2: CONNECT b:443
  L1->>T: T-tunnelId-1 <-> E1 建连
  L2->>T: T-tunnelId-2 <-> E2 建连
  E1->>E1: 校验 own allow -> net.Dial a
  E2->>E2: 校验 own allow -> net.Dial b
  C1<->>L1<->>T<->>E1: 双向字节(tunnel-1)
  C2<->>L2<->>T<->>E2: 双向字节(tunnel-2)
```

### 输出结构

仅对「SOCKS5 基座」未落在多并发上的薄层：

```
cmd/relay/main.go                 # relay tunnel 增加 --watch(复用 resolveWorkspaceID)
internal/relay/backend/relay.go   # 握手时把 resolve 出的 watch 放入 Connect 头
internal/relay/protocol/message.go# (依赖 SOCKS5)TunnelConnectRequest 头部带 watch 字段
internal/relay/server/client.go   # handlerTunnelConnect 按 watch 取 executor 路由
internal/relay/integration_test.go# 多 executor + 并发多隧道端到端
docs/relay-protocol.md            # 同步多 executor/隧道选工作区说明
```

### 实施单元

#### U1. `relay tunnel` 加 `--watch` 工作区选择器

**Requirements:** R3, R4

**Files**
- modify `cmd/relay/main.go` — 加 `tunnelCmd.Flag("watch").HintOptions(...)`
- modify `internal/relay/backend/relay.go` — 隧道建立前置把 `resolveWorkspaceID` 结果随请求发给（解析失败/无 executor→fail）

**Approach**
- 新增 `tunnelWatch` flag，命名与 `--watch`（同 `exec`/`push`）。
- 隧道启动时 `resolveWorkspaceID(cfg, *tunnelWatch)` → `watchID` 作为 `Connect 头部`；缺省目录当前目录名推断。
- 选中 watch 无在线 executor：隧道启动/首次 CONNECT 即错误输出，非零退出（命令行可提前查 `status` 确认）。

**Test**
- U1-T1 显式 `--watch site-a` → 隧道即打到 site-a 出口。
- U1-T2 缺省（目录名==site-b）→ 打 site-b。
- U1-T3 目录名不匹配任一 watch → 明确报错（复用现有解析错误文案）。
- U1-T4 `--watch site-c`（无执行方）→ fail，报「无在线执行方」。

#### U2. 协议/转发：`MsgTunnelConnect` 头部带 watch，中转按 watch 路由

**Requirements:** R3, R4

**Files**
- modify `internal/relay/protocol/message.go` — `TunnelConnectRequest` 补 `Watch string \`json:"watch,omitempty"\``（SOCKS5 基座落地后补）
- modify `internal/relay/server/client.go` — `handleTunnelConnect` 用 `watch` 求 `GetExecutor(watchID)`，无→fail；沿用 `forwardTo` executor 的一段路由。

**Approach**
- 头带 watch 字段；中转收到 `MsgTunnelConnect` 后 `GetExecutor(watchID)` 拿到 executor，成立隧道注册表条目并转发（与 `forwardToExecutor` 的 watch→executor 逻辑同构）。
- 无执行方 fail-fast，不建立隧道注册表条目。

**Test scenarios**
- U2-T1 watch 有执行方 → 建立隧道并转发。
- U2-T2 watch 无执行方 → CONNECT fail、不立条目。
- U2-T3 消息往返 RoundTrip 携带 watch 字段。

#### U3. 并发安全 + 集成：多 executor 多隧道并行端到端

**Requirements:** R1, R5, R6

**Files**
- modify `internal/relay/integration_test.go` — 双 executor / 双隧道端到端
- modify（必要时）`internal/relay/server/server.go` / `client.go` — 隧道表与 `executorMu` 协同（确保并发锁不遗漏）

**Approach**
- 集成：注册两个 executor（site-a、site-b），各写各的白名单，本地同起两条 `relay tunnel`，分别请求各自放行的目标并在连任一端断开只影响本隧道。
- 断言：双向字节互不串；任一条 `MsgTunnelEnd` 只拆除自身表 `tunnelID`；`tunnelID` 计数不泄漏。

**Test scenarios**
- U3-T1 两隧道同起同时可用（各自放行报）。
- U3-T2 关一条另一条仍存活。
- U3-T3 并发 Race 覆盖（多隧道注册/拆除），`-race` 绿。

#### U4. 文档同步

**Files**
- modify `docs/relay-protocol.md` — 新增「多 executor 与工作区隧道选择」小节（`--watch` 语义、台账展示、并发说明）。
- modify `README.md` — 双机双隧道示例（`relay tunnel --listen ... --watch site-a` + `--watch site-b`）。

**Approach** 仅同步既有文档，不虚构。

---

## Verification Contract

- 单元/集成：`make test`（`go test -v -race ./...`）必须全绿。
- 隧道/多 executor 集成：`go test ./internal/relay/...`；并发场景带 `-race`。
- 端到端冒烟（建议）：两台机器（或本地两 watch 模拟）各跑 `relay watch`；本地同时：
  - `relay tunnel --listen 127.0.0.1:1080 --watch site-a` + `curl --socks5-hostname 127.0.0.1:1080 <site-a放行>`
  - `relay tunnel --listen 127.0.0.1:1081 --watch site-b` + `curl --socks5-hostname 127.0.0.1:1081 <site-b放行>`
  - 两者并行成功、关其一另一仍通。
- 红线（不改变）：`MsgTunnel*` 语义、白名单 executor 强制、中转只转发不落地/不本地执行、单 watch 一对一。

## Definition of Done

- 一台中转、两台机器两 executor 在线；`--watch` 可选指定出口、缺省回退目录名。
- 并发多隧道各自独立、互不串扰、关闭不影响它隧道（AE 场景过）。
- 相关 `go test` 全绿；`-race` 无并发错误。
- 文档（协议/README）同步多 executor 与工作区隧道选择。

各单元 DoD：
- U1：`--watch` 可选集成 executor；缺省推断、无执行方报错。
- U2：`Connect` 头部带 watch，中转按 watch 路由，无执行方 fail-fast。
- U3：多 executor 多隧道集成 + Race 绿。
- U4：文档同步。