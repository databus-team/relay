---
artifact: ce-plan/v1
---

# 后台端口转发 daemon 聚合 — 实施计划（rev3，docker 对齐、无新子命令）

前置：`docs/plans/2026-10-10-2349-feat-detach-portforward-daemon-requirements.md`（requirements-only）+ rev2 评审记录。

## Goal

`relay run -d -p <spec>...` 使本地 `-p` 端口在 CLI 退出后仍可达（远程执行、端口转发到本地）。架构对齐 docker：**控制面集中、运行时独立**——本机一个**隐式常驻 forward daemon**（dockerd 角色）集中持有端口映射表、做端口分配/冲突检测（`net.Listen`），为每个 spec 在 daemon 内起一个**独立转发单元**（docker-proxy 角色，各自 `TunnelOpenPortForward` 连远端 executor），`relay stop <taskid>` / `relay ps -a` 复用既有命令联动。**不新增任何用户可见子命令**（延续把 pf 拆掉合入 run 的决策）。前台 `run -p` 语义不变。

## 架构（理由与 docker 对齐）

- **控制面单 daemon、运行时独立**：daemon 集中建 listener（统一端口分配/冲突检测），每条映射独立 goroutine 转发（互不阻塞、可各自连不同远端 executor）。
- **无新子命令 UX**：发布=`relay run -d -p`，查询=`relay ps -a`（PORTS 列扩展），停止=`relay stop <taskid>`（联动）。端口映射是 task_id 的属性（docker：端口是容器属性）。
- **daemon 退出即回收**：本机 daemon 进程消亡时其所有 listener/转发 goroutine 随之回收（进程级清理，docker Pdeathsig 语义）；不遗留孤儿监听。
- **detach 无关端口**：`-d` 只约束远端任务在前台/后台；端口表由本机 daemon 持有，不随 CLI 进程生存。

## 关键决策（已拍板）

1. **无 `forward`/`pf` 顶层子命令**：内部 daemon 仅以隐藏子命令入口（如 `relay __forward_run`，不进文档/不做补全引导）被 `run -d -p` 经 `daemonStartArgs` 隐式拉起。
2. **先任务、后登记**：detach 调用 `ExecStreamWithPorts(detach=true, ports=...)` → onChunk 拿到 TaskID（后端 relay.go:741 回写）→ 再把 specs 经 IPC 登记进本机 daemon（带 task_id）。任务启动失败则不登记任何 spec。
3. **同端口冲突=拒绝**：daemon 对 local_port 冲突返回错误，拒绝该 spec，不影响其它已绑定规则。
4. **stop 联动摘除**：`relay stop <taskid>` 远端成功后，内部 IPC 通知本机 daemon 按 task_id 移除对应转发。
5. **daemon 重启显式告警**：daemon 重启丢失规则时，经 `relay ps -a` 或日志提示「规则已丢失、远端任务可能仍在运行」；恢复须先 `relay stop` 旧任务再重新 `run -d -p`（避免泄漏）。
6. **孤儿检测**：`TunnelOpenPortForward` 远端断连时标记该规则失效并给清理入口（`relay ps -a` 可见），不默默长挂。

## Context（代码锚点，已核实）

- 断点：`cmd/relay/main.go:1197` `if len(pfSpecs) > 0 && !*execDetach`（detach 跳过本地 forwarder）。
- detach 执行后端：`internal/relay/backend/relay.go:733` `StartTaskWithPorts`，TaskID 回写 `:741`（`sess.Write(true, task.Meta().ID)`，CLI 经 onChunk 拿到）。
- 复用通道：`internal/relay/backend/relay.go:414` `TunnelOpenPortForward`（跳过 network_allow）；`internal/relay/client/tunnel.go:326` `Client.TunnelOpenPortForward`。
- 隐式 daemon 拉起：`internal/daemon/daemon.go` `PidFile/LogFile/Start/Get/Stop`；`cmd/relay/main.go:362` `daemonStartArgs`（接受任意 args，`daemon.Start` 会 `exec.Command(exe, args...)`）。参考 tunnel daemon 范式 `:456` `tunnelInstanceName` + `daemonStartTunnel`。
- 端口转发实现：`cmd/relay/portforward.go:102` `StartPortForwardsForeground`、`:142` `handlePortForwardConn`（本地监听 loop + 双向泵），从中抽出可复用单元。
- `relay ps` PORTS 列：`cmd/relay/main.go:1379`（已有，扩展显示本机转发）。

## Scope / Non-goals

### In scope
1. **隐式 forward daemon**：内部进程（隐藏子命令入口），由 `run -d -p` 检测到 `len(pfSpecs)>0 && detach` 时 `daemonStartArgs` 拉起。持有端口映射表 + 统一端口分配/冲突检测。
2. **IPC 控制通道**（内部，非用户命令）：`run -d -p`/`stop <taskid>` 经 unix socket 与 daemon 通信（register/unregister/list），实现精确增删。
3. **`run -d -p` 接入**：detach + `-p` → ensure daemon → 启动任务拿 TaskID → 经 IPC register（带 task_id）→ 打印 TaskID。
4. **`relay stop <taskid>` 联动**：远端 stop 成功后经 IPC unregister 摘除对应本机转发。
5. **`relay ps -a` 增强**：展示本机活跃转发规则（含失效标注）。

### Non-goals
- 不新增用户可见的 `forward`/`pf` 子命令（延续拆 pf 决策）。
- 不做规则跨重启持久化/自动重建（重启后告警 + 手工重新 run）。
- 不改前台 `run -p` 语义（仍 CLI 自持，复用 `StartPortForwardsForeground`）。
- 不在远端节点新增常驻 forward 组件（远端任务由 supervisor 管，端口发布集中在本机 daemon）。
- 不做本机之外的端口发布（`-p` 的"本地"= 执行 CLI 的这台机器）。

## Work 拆解

### Unit 1：隐式 forward daemon（隐藏入口 + 主循环 + 端口表）
- **改** `cmd/relay/main.go`：新增隐藏子命令（如 `relay __forward_run`，不进文档/补全），进入 daemon 主循环；dispatch case。
- **新增** `cmd/relay/forward.go`：`runForwardDaemon()`（加载 config → getRelayBackend → 监听 unix socket → accept 循环）、`daemonEnsureForward`（用 `daemonStartArgs` 隐式拉起）。
- **验**：`daemonStartArgs` 拉起后 `ps` 见 `relay __forward_run` 进程；`daemon.Get` 存活；socket 就绪。

### Unit 2：IPC 协议 + 转发单元（dockerd/docker-proxy 分工）
- **改** `cmd/relay/forward.go`：unix socket `~/.relay/forward.sock`（启动前 `os.Remove` stale）；JSON 行式消息（op=register/unregister/list，含 executor/spec/bind/local_port/task_id）。
- **改** `cmd/relay/portforward.go`：抽出复用单元 `startOneForward`（建 listener + 双向泵），供 daemon 与前台共用。
- **改** `cmd/relay/forward.go`（或 portforward.go）：register 检测 local_port 冲突即拒绝；`TunnelOpenPortForward` 远端断连标失效（决策 6）。
- **验**：register→本地 listen→连远端 echo→unregister→listen 关闭；同端口二次 register 被拒；远端断连标失效。

### Unit 3：`run -d -p` 接入（先任务后登记）
- **改** `cmd/relay/main.go` `runExec`：`*execDetach && len(pfSpecs)>0` → `daemonEnsureForward` → `ExecStreamWithPorts(detach=true, ports)` 拿 TaskID（onChunk）→ 逐 spec 经 IPC register（带 task_id）→ 打印 TaskID。
- **验**：`relay run --node hub -d -p <lp>:<rp> '<server>'` 返回 TaskID 后 `lsof -iTCP:<lp>` 有监听、`curl` 可达。

### Unit 4：`relay stop` 联动摘除
- **改** `cmd/relay/main.go` `runStop`：远端 stop 成功 → 经 IPC unregister（按 task_id）摘除本机对应转发；失败仅告警（决策 4）。
- **验**：detach 任务 `relay stop <taskid>` 后对应本地端口 listen 关闭；未绑定规则不被误删。

### Unit 5：`relay ps -a` 增强 + 多 spec + 回归 + 重启告警
- **改** `cmd/relay/main.go` `runPs`：汇总展示本机活跃转发规则（含失效标注）。
- **验**：`-p A:B -p C:D` 单 daemon 进程多转发；`ps -a` 列多条；daemon 重启 `ps -a` 告警；前台 `run -p` 回归。

## Acceptance gates（可验证命令）

```bash
# G1 detach 本地转发可达
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'
lsof -nP -iTCP:18083 -sTCP:LISTEN
curl -s http://127.0.0.1:18083/

# G2 多 -p 单 daemon（独立本地端口避免冲突）
./relay run --node hub -d -p 18084:8899 -p 18085:8899 'python3 -m http.server 8899'
ps aux | grep '[r]elay __forward_run' | wc -l   # == 1（仅隐式 daemon）
./relay ps -a                                   # 列多条转发规则

# G3 stop 联动（G1 任务记为 T1）
./relay stop T1
lsof -nP -iTCP:18083 -sTCP:LISTEN              # 不再监听

# G4 同端口冲突拒绝
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'  # 二次 register 18083 被拒

# G5 前台回归
./relay run --node hub -p 18086:8899 'sleep 5'   # 前台仍监听，无回归

# G6 无新增用户子命令
./relay --help 2>&1 | grep -iE 'forward|pf'      # 不得出现 forward/pf 顶层命令（仅隐藏 __forward_run）
```

## 风险 & 回滚

- **风险 1 双份转发实现**：daemon 与前台各写一套 → 抽共用 `startOneForward`（Unit 2）。
- **风险 2 socket 残留/权限**：stale socket 致 register 失败 → 启动前 `os.Remove`。
- **风险 3 stop 联动不同步**：远端已停但本机 unregister 失败 → 规则残留，`ps -a` 可见 + 失效标注（决策 6），用户可手工清理。
- **风险 4 daemon 重启丢规则**：远端任务仍跑但本机端口丢失 → `ps -a` 明示；恢复先 `relay stop` 旧任务再重跑（避免泄漏）。已列决策 5。
- **回滚**：`git revert` 相关 commits；前台 `-p` 与 detach 路径分离，回滚 detach 不影响前台。

## Validation plan

1. `go build ./... && go vet ./...`（改动包）。
2. `go test ./cmd/relay/... ./internal/relay/backend/...`（新增 IPC/转发测试，起 in-process hub + echo）。
3. Acceptance gates G1–G6 全过。
4. 回归：现有 tunnel daemon 不受影响（`relay tunnel status` 正常）。
