---
title: 隧道并入 node + 统一 ps 控制面 - Plan
type: feat
date: 2026-10-12
topic: tunnel-into-node
execution: code
---

# SOCKS5 隧道并入 node daemon + 统一 `ps` 控制面

## Goal Capsule

- **Objective:** 把 SOCKS5 出网隧道从「每出口一个独立 `tunnel-<egress>` daemon」收敛进本机 `relay node`（对齐已落地的 port-forward holder）。隧道成为 node 持有的常驻单元，意图持久化到状态文件 `~/.relay/tunnels.json`（docker-run 式），node 重启后按 `restart` 策略回放。隧道并入 `relay ps`/`relay stop`/`relay logs` 统一控制面。
- **Product authority:** 不新增协议消息、不改 `MsgTunnel*` / executor `network_allow` / 中转只转发不落地等既有决策；只把「隧道由谁常驻持有 + 怎么持久 + 在哪盘点」收拢到 node。**detach 任务 `--restart`（executor 侧 supervisor）另立后续计划，本期不实现**。
- **Open blockers:** 无。决策已确认：状态文件持久（docker-run 式）；`restart` 默认 `no`；`relay stop` = kill 意图；保留 `relay tunnel start` 创建入口。

---

## 关键决策（已确认）

1. **状态文件 `~/.relay/tunnels.json`** 是隧道意图唯一持有点（docker run 运行时状态，非 compose config）。不写入共享 `config.yaml`，避免 `relay sync` 连带分发到执行端。（session-settled: user-directed）
2. **`restart` 策略对齐 `docker run --restart=`**；恢复事件是 **node 重启/崩溃**（隧道是 node 进程内 goroutine，隧道自身 crash 属 node 层问题）。默认 `no`。（session-settled: user-directed）
3. **`relay stop` 即 kill 意图**：隧道 stop = 删状态条目（`docker rm` 语义）。（session-settled: user-directed）
4. **分动词分层**（不把创建塞进 `ps`）：`start` = 创建/配置；`ps` = 只读盘点；`stop`/`logs` = 运维。
5. **node 是状态文件唯一写者**（经 `tunnel.sock` 串行化，避免 CLI 竞态）；CLI 只发控制消息。
6. **以 egress 作为隧道可寻址身份**（`relay stop intranet`）；每出口单隧道，多端口/出口再演化。

### restart 语义

恢复事件是 **node 进程的下一次启动**（隧道是 node 进程内 goroutine，隧道自身 crash 属 node 层问题）。注意：**node 无常驻崩溃看护**——daemon 仅 pid 探活 + detached 启动，不自启、无 recover 兜底，因此机器重启或 node 崩溃后需人工 `relay node start`（或 `restart`/`upgrade`）才会回放；node 自启/看护列为后续计划。

| policy | node 下一次启动后 |
|---|---|
| `no`（默认） | 保留在状态文件、`ps -a` 标 `stopped`，**不**自动拉起 |
| `always` | 自动重新打开监听 |

### 生命周期

| 操作 | 效果 |
|---|---|
| `relay tunnel start [-w egress] [--listen addr] [--restart=always\|no]` | upsert 状态条目 + node 打开。默认 `no`。|
| `relay tunnel stop [-w egress]` | 关闭 + **删除条目**（intent 移除）。|
| `relay tunnel restart -w <egress>` | 关闭并**重开**同一 egress 的监听、**保留状态条目**（不必重输 `--listen`/`--restart`）。经 `tunnel.sock` 的 `restart{egress}` 转发。|
| `relay stop <egress>` | 同 `relay tunnel stop`（kill 意图）。|
| node 启动 | 回放状态文件：`always` 自动打开；`no` 保留标 `stopped`。|

> **node 重启会断开全部隧道**（同一 `node` 进程内持有），并连带断开全部 detach 端口转发；因此对隧道有依赖时推荐 `--restart=always`，无依赖时用 `relay tunnel restart -w <egress>` 按条重开，避免逐个 `start` 重忆参数。

---

## Requirements

### R1. node 内 `tunnelHolder`（对标 forwardHolder）
- 在 agent 角色 goroutine 启动（与 forwardHolder 并列）。监听 `~/.relay/tunnel.sock`，收 JSON 行式控制消息。
- 控制操作：`open{egress,listen,restart}` / `close{egress}` / `restart{egress}` / `list`。`restart{egress}` = 关闭该条监听并重开、保留状态条目。
- 持有：`byEgress map[string]*tunnelEntry`（egress → 条目）、每条目运行时 `net.Listener` + accept/pump goroutine、起止时间。
- 状态持久化：变更在 `mu` 锁内写 `~/.relay/tunnels.json`（tmp/rename 原子写）。启动时 `restore()` 回放。
- 复用 SOCKS5 逻辑：`handleTunnelConn`/`socks5Handshake`/`socks5Reply`（`cmd/relay/tunnel.go`）。
- **非 loopback 告警不丢失**：`open` 与 `restore` 在绑定非 loopback 地址时必须重新发出与现 `runTunnel` 一致的安全告警（复用 `isLoopbackAddr`）。该告警文本原在被移除的 `runTunnel` 中，迁移时不得静默丢失——否则非 loopback 监听会在 node 重启回放时被无声恢复。
- **验**：状态文件往返；并发 open/close 幂等；`restore` 回放 `always`、跳过 `no`；`open`/`restore` 绑定非 loopback 时均发出告警。

### R2. CLI `relay tunnel` 收口
- `start`/`stop`/`status` 经 `tunnel.sock` 与 node 通信。**仅 `start` 前置 `ensureLocalRelayNode`**（必要时拉起 node）；`status` 在 node 未运行时直接报「node 未运行，无隧道实例」、不拉起 node；`stop` 在 node 未运行时视为已完成（无条目可删）。避免只读/停止动词因拉起 node 而触发 `always` 回放、反向把隧道打开。
- `start` 校验 egress（`tunnelRequireExecutor`）；新增 `--restart`/`--no-restart`。
- `status` 复用 `tunnel_status.go` 渲染（表格 + `--json` + cluster 出口链路探测）。
- `run` 移除（常驻交给 node）；**保留 `restart -w <egress>`**（经 `tunnel.sock` 的 `restart{egress}` 关闭并重开该条 egress，语义与旧 `relay tunnel restart` 等价，无需重输 `--listen`/`--restart`）；`upgrade` 归 `node upgrade`；`logs` 路由到统一 `relay logs`。

### R3. `relay ps` / `stop` / `logs` 统一
- `runPs` 在拉取远端 detach 任务后，若本机 node 在跑则经 `tunnel.sock` 拉隧道列表，合并渲染。
- **远端拉取失败不丢本机隧道**：现 `runPs` 在 TaskList 出错时直接退出；改为远端任务拉取失败仅降级（告警）而不中断，仍输出本机隧道行——隧道疑似出问题时正是中转可能不可达之刻。
- **`ps -a` 语义**：隧道 `stopped`（`restart=no` 未运行）仅在 `ps -a` 中列出，与远端任务行/`docker ps` 一致；`ps` 默认只列 running。
- 输出加 `TYPE` 列；`--json` 带分类。隧道行复用 PORTS 列显示 `listen`、COMMAND 显示 `egress <egress>`、STATUS 显示 `running`/`stopped`。
- `runStop`：TaskList 未命中 → 回退本地隧道 `close`。
- `runLogs`：TaskID 未命中 → 回退读 node 日志中 `[tunnel <egress>]` 前缀段（该前缀为**新增、由 holder 统一输出**，见 Unit 1；node 前台运行、stderr 未落 `~/.relay/node.log` 时该回退降级为提示）。
- 兼容：node/holder 未运行则 `ps` 仅远端任务；node 在跑但中转不可达时仍列出本机隧道（如旧）。

### R4. 配置/文档
- README + CONCEPTS.md 增补「隧道 / restart 策略 / 状态文件」名词；`config.example.relay.yaml` 注释补充（不改 config 结构）。

---

## 实现单元（依赖顺序）

### Unit 1: `cmd/relay/tunnel_holder.go`（新）
- `tunnelHolder`：`Run(ctx)` 监听 socket + `restore()`；`handleConn` 逐条收 JSON；`dispatch`（open/close/restart/list）。`restart{egress}` = 关闭该条监听并重开（保留状态条目）。
- `tunnelEntry`：`{Egress, Listen, Restart}` + 运行时 `listener net.Listener`、**在途连接集合**、`state`、`started time.Time`。
- 状态文件读写：`loadTunnels`/`persistTunnels`（tmp/rename）到 `~/.relay/tunnels.json`。
- 控制消息 wire：`tunControlMsg{tunControlResp}`（对齐 `fwdControlMsg` 风格）。
- **日志前缀**：holder 统一以 `[tunnel <egress>]` 前缀输出该条目的所有日志（open/close/error，以及 `handleTunnelConn` 内的连接级错误——需把其日志出口改为经 holder 包装，不再直接 `fmt.Fprintf(os.Stderr, ...)`），作为 `relay logs <egress>` 回退的过滤依据；`cmd/relay/tunnel.go` 现有 `[tunnel]` 行不满足该前缀。
- **stop/upsert 切断在途连接**：`tunnelEntry` 记录已 accept 的连接，`close`/`upsert` 时一并关闭，使 stop 对在途连接立即断开（与「kill 意图」一致）。
- **非 loopback 告警**：`open`/`restore` 绑定非 loopback 地址时复用 `isLoopbackAddr` 发出与现 `runTunnel` 一致的安全告警（该文本随 `runTunnel` 一并移除，须在 holder 侧重建）。
- **验**：状态文件往返 + 并发 open/close 幂等 + stop 后已建连接被关闭 + 日志行带 `[tunnel <egress>]` 前缀 + 非 loopback 绑定发出告警。

### Unit 2: node 启动接入
- `node.go` agent goroutine 内（forwardHolder 旁）新建 `tunnelHolder` 并 `go holder.Run(ctx)`。
- **验**：node 起后 `tunnel.sock` 就绪；`restore()` 重开 `always`；`lsof` 见监听。

### Unit 3: CLI `relay tunnel` 重接线
- `main.go` tunnel dispatch 改为经 `sendTunnelControl` 走 socket；移除 `run`/`upgrade`/`logs` 分支，**保留 `start`/`stop`/`status`/`restart`**（均经 socket）。
- 仅 `start` 前置 `ensureLocalRelayNode`；`status`/`stop` 不拉起 node（见 R2）。
- **旧 daemon 退役**：`start`（holder `open`）在 bind 失败时探测残留的 `~/.relay/tunnel-<egress>.pid`，停掉该旧实例或打印确切的 kill 命令；过渡期保留 `daemonStop(tunnelInstanceName(target))` 路径供 `relay tunnel stop` 清理旧进程。
- **验**：`relay tunnel start -w intranet --listen 1080` 后 lsof 有监听、curl 通；`status` 对齐表格；`stop` 后监听消失；存在残留旧 daemon 时报出确切清理方式而非裸 bind 错误。

### Unit 4: `relay ps` / `stop` / `logs` 合并
- `runPs` 合并隧道行 + TYPE 列；隧道 `stopped` 仅在 `-a` 列出；远端任务拉取失败不中断、仍输出本机隧道行。
- `runStop`/`runLogs` 加隧道回退。
- **验**：`ps` 同列 detach+tunnel；`ps`（不带 `-a`）不显示 stopped 隧道；中转不可达时 `ps` 仍列出本机隧道；`stop intranet` 关隧道；`logs intranet` 出隧道日志。

### Unit 5: 复用与收尾
- 独立或复用 `rb`（对齐 forwardHolder 现状）；文档 + CONCEPTS.md + README。

---

## 核心复用

- `cmd/relay/tunnel.go`：`handleTunnelConn` / `socks5Handshake` / `socks5Reply`。
- `cmd/relay/forward_holder.go`：`forwardHolder` 控制 socket 结构与 `sendForwardControl` 模式（照搬为 tunnel 版）。
- `cmd/relay/tunnel_status.go`：表格/JSON 渲染 + `queryClusterStatusSafe` 出口链路探测。
- `internal/relay/backend/relay.go`：`TunnelOpen`（不变）。
- `internal/daemon`：`SockFile`/`PidFile`/`LogFile`、`daemon.Get` 探活 node。
- `cmd/relay/main.go`：`ensureLocalRelayNode`、`resolveNodeTarget`、`getRelayBackend`。

---

## 兼容 & 回滚

- 新增状态文件 + socket 是附加资源；**不自动迁移旧 `tunnel-<egress>` 进程**。升级前置步骤：先停掉旧 daemon（过渡期 `relay tunnel stop` 仍可停旧实例，或 `kill $(cat ~/.relay/tunnel-<egress>.pid)`）再 `relay tunnel start` 重建；否则旧进程仍占 `--listen` 地址、重建将在 bind 阶段失败。`start` 亦应在 bind 失败时探测并报出残留实例的确切清理方式（见 Unit 3）。
- `relay ps` 纳入隧道依赖本机 node 在跑；不在跑则仅远端任务（如旧）。
- 回滚：**本期主线已移除 `relay tunnel run`**；回滚需重新恢复该前台模式（`tunnel run` 分支 + `daemonStartTunnel`）再撤 `tunnel_holder.go`、恢复 node.go 不启动 holder 并回退 `ps`/`stop`/`logs` 行为。

---

## Follow-up（后续独立 plan）

- **detach 任务 `--restart`**：`TaskMeta.Restart` + supervisor 崩溃退避重启 + executor 重启重拉；protocol/backend 透传；`ps` 展示 RESTART 列。将 `restart` 真正泛化为跨 detach/tunnel 的通用策略（本期仅 tunnel 实现，但顺应同一语义）。
