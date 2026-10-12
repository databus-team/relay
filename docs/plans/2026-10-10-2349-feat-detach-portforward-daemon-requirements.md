---
artifact_contract: ce-requirements/v1
readiness: requirements-only
---

# 后台端口转发 daemon 聚合 — 需求澄清

## Goal Capsule

`relay run -d -p <spec>...`（detach 后台运行 + 端口发布）目前只有任务在远端存活，**本地端口从不绑定**；前台 `run -p` 正常。目标是让 detach 场景的 `-p` 在 CLI 退出后**本地端口保持可达**，且采用**本机单一常驻 forward daemon 聚合所有本机 port-forward channel** 的架构，而非逐个 spec 起进程。

## 背景事实（已核实）

- 前台 `run -p` 生效；detach `run -d -p` 不生效。
- 根因：`02f180c`（移除独立 `pf` 子命令）删掉了 detach 分支的 `StartPortForwardsBackground`（原为每个 spec 起一个 `pf` daemon），代码只剩 `cmd/relay/main.go:1197` 的 `if len(pfSpecs) > 0 && !*execDetach` 前台分支。
- `internal/relay/backend/relay.go:414` `TunnelOpenPortForward`（跳过 network_allow）已存在，可直接复用。
- 本机 `CT-MBP.local` 已运行两个 **tunnel** daemon（`tunnel-extranet`/`tunnel-intranet`，SOCKS5），**无** forward/port-forward daemon。

## Product Contract

### Scope（做什么）

1. **新增本机常驻 `forward` daemon**（复用 `internal/daemon` + `daemonStartArgs`，参照 tunnel daemon 模式）：
   - 单进程持有**一个本机**上的所有 port-forward channel（无论目标 executor 是 extranet/intranet/hub）。
   - 通过 `TunnelOpenPortForward` 建立每个 spec 的远程转发；本地监听 loop 挂在该 daemon 进程，CLI/前一个 detach run 退出后仍存活。
   - 进程标识：单实例名（如 `forward`），pid/log 落 `~/.relay/`（PidFile/LogFile）。
   - 子命令：`forward run|start|stop|status|restart`，对齐 tunnel 范式。
2. **`run -d -p` 惰性拉起 + 登记**：
   - 执行 detach 且带 `-p` 时，先确保本机 forward daemon 已运行（不在则 `daemonStart` 拉起）。
   - 经**本机 unix domain socket** 把每个 spec 登记进 daemon（daemon 上已含 spec 解析与启动监听）。
   - 非 detach 的前台 `run -p` 不变，仍由 CLI 进程自持（`StartPortForwardsForeground`）。
3. **生命周期跟随 run 后台任务**（参考 docker cli 设计）：
   - 规则不磁盘持久化；daemon 重启后规则丢失（需重新 `run -d -p`）。
   - detach 任务 stop 时，对应本地转发规则联动移除（对 unix socket 发 remove/按 task-id 清理）。

### Out of Scope（不做什么）

- 不重建 `relay pf` 子命令；不做规则的跨重启持久化/自动重建。
- 不改变前台 `run -p` 的 CLI 自持语义与端口转发逻辑。
- 不引入新的远端节点侧常驻组件（远程 daemon 已存在不含 forward 角色）。
- 不在本机之外的目标机器上做端口发布（`-p` 的"本地"= 执行 CLI 的这台机器）。

### 成功标准（可验证）

1. `relay run --node hub -d -p <lp>:<rp> '<server>'` 返回 TaskID 后：
   - `lsof -iTCP:<lp>` 本地存在监听；
   - 服务可达：`curl http://127.0.0.1:<lp>/` 返回预期内容。
2. 多个 `-p`（如 `-p A:B -p C:D`）在一个台机上**同一 forward daemon** 内建监听（`ps` 显示单进程、无多个 forward 进程）。
3. `relay forward status` 显示本机 forward daemon 运行态与已登记规则。
4. detach 任务 `relay stop <taskid>` 后，对应本地转发端口被移除（监听关闭）。
5. 回归：前台 `relay run -p` 仍正常（未破坏既有行为）。

### 开放问题（留给 ce-plan / 实现阶段确认）

1. **规则 ↔ 任务的关联键**：以 detach 返回的 TaskID 为 key 管理规则与清理（推荐），还是以端口为 key？
2. **unix socket 控制协议**：add/remove/list/status 的命令与响应格式（JSON 行式，参考现有 exchange/cmdfile）。
3. **stop 联动触发方**：`relay stop` 是对远端 supervisor 发起，本地 daemon 不在远端——本地清理动作由谁触发（CLI 侧 stop 成功后再对本机 daemon 发 remove，还是 daemon 通过节点生命周期感知）。
4. 同端口二次 `-p` 的冲突行为（覆盖 / 报错）。

## Notes

- 本机 forward daemon 通过 unix domain socket 与 CLI 通信（两者同机）；若未来跨机/多用户再演进为 WS 登记，protocol 另议。
- 复用 `internal/relay/backend/relay.go` 的 `TunnelOpenPortForward` 与现有 `client/pool.go` 连接复用，避免重复实现 channel。
