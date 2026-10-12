---
artifact: ce-plan/v1
---

# 后台端口转发 daemon 聚合 — 实施计划（rev2，docker 控制面对齐）

前置：`docs/plans/2026-10-10-2349-feat-detach-portforward-daemon-requirements.md`（requirements-only，已过 ready-for-planning）。

## Goal

`relay run -d -p <spec>...` 使本地 `-p` 端口在 CLI 退出后仍可达。架构：**控制面单 daemon、运行时分布式**——本机一个常驻 `forward` daemon 统一持有并管理所有本机 port-forward 映射表（docker dockerd 语义：单守护进程集中登记/增删端口发布），而每条通道的实际建立/转发分布式地连到各远端 executor（extranet/intranet/hub）。`run -d -p` 惰性拉起 daemon 并经 socket 动态登记/移除规则，`stop <taskid>` 按 task_id 精准摘端口。前台 `run -p` 语义不变。

## 关键决策（用户已拍板）

1. **任务先启动、再登记**：detach 调用 `ExecStreamWithPorts(...)` 且带 `-p` 时，先启动远端任务拿到 TaskID（经 onChunk 流式回调 `sess.Write(true, task.Meta().ID)`，见 `internal/relay/backend/relay.go:741`），**拿到 TaskID 后**再逐 spec 向本机 forward daemon 发 `add`（含 task_id）。任务启动失败则一个 spec 都不登记，无回滚负担。
   - 依据：`ExecStreamWithPorts` detach 分支远端在 `relay.go:733` 调 `sup.StartTaskWithPorts(...)`，`:741` 回写 TaskID；CLI 的 onChunk 收到即知晓。故「先任务后登记」语义自洽，无需拆两段 bind+关联。
2. **同端口冲突 = 拒绝**：`add` 检测到 `local_port` 已被占用（另一规则或本机无关进程）即报错拒绝该 spec，不静默覆盖、不影响其它已绑定规则。
   - 依据：对齐全 docker `-p` 语义（端口被占报错）；且单 daemon 聚合下若允许覆盖，stop 联动无法精确匹配。
3. **控制面 socket 保留，运行时无状态**：daemon 经本机 unix socket 接收 add/remove/list/status 动态管理规则（docker 语义需要的动态增删/精准摘除）；每条通道的转发由 daemon 各自 `TunnelOpenPortForward` 分布式建立。
4. **daemon 重启不自动恢复规则、但显式暴露**：daemon 重启/崩溃丢失已登记规则时，`forward status` 给出「规则已丢失、远端任务可能仍在运行」的告警；恢复须先 `relay stop` 旧任务再重新 `run -d -p`（避免泄漏）。
5. **孤儿监听处理**：`TunnelOpenPortForward` 远端通道断连时，daemon 标记该规则为失效并提供清理入口（`forward status` 可查可见），不默默长挂。

## Context（贴代码锚点）

- 断点：`cmd/relay/main.go:1197` `if len(pfSpecs) > 0 && !*execDetach`（detach 跳过本地 forwarder）。
- detach 执行：`internal/relay/backend/relay.go:733` `sup.StartTaskWithPorts(...)`；TaskID 回写 `:741`。
- 复用通道：`internal/relay/backend/relay.go:414` `TunnelOpenPortForward`（跳过 network_allow）；`internal/relay/client/tunnel.go:326` `Client.TunnelOpenPortForward`。
- daemon 基建：`internal/daemon/daemon.go` `PidFile/LogFile/Start/Get/Stop`；CLI 侧 `daemonStartArgs`（`main.go:362`）。
- 参照范式：tunnel daemon（`cmd/relay/tunnel.go` `runTunnel` + `main.go:456` `tunnelInstanceName` + `daemonStartTunnel/StopTunnel/StatusTunnel`）。
- 端口 forwarder 实现：`cmd/relay/portforward.go` `StartPortForwardsForeground(:102)` + `handlePortForwardConn(:142)`（本地监听 loop + 双向泵）。

## Scope / Non-goals

### In scope
1. 本机常驻 `forward` daemon（单实例名 `forward`，pid/log 落 `~/.relay/`），子命令 `forward run|start|status` + 控制面 pod `remove` 不需 CLI 子命令。
2. `forward run` 前台主循环：监听本机 unix socket（`~/.relay/forward.sock`），接收 add/remove/list/status 控制消息；每个 spec 建本地 TCP listener + `TunnelOpenPortForward` 远端通道，双向泵。
3. `run -d -p` 惰性拉起：detach 且带 `-p` 时，ensure daemon running（`daemonStartArgs` 拉起），先启动任务拿到 TaskID，再经 socket 逐 spec 登记（add 带 task_id）。前台 `run -p` 保持 `StartPortForwardsForeground` 不动。
4. 生命周期跟随任务：规则不持久化；`relay stop <taskid>` 成功后经 socket 按 task_id 发 `remove` 精确摘除。

### Non-goals
- 不重建 `relay pf` 子命令。
- 不做规则跨重启持久化/自动重建（重启后 `forward status` 告警 + 手工重新 `run -d -p`）。
- 不改前台 `run -p` 语义。
- 不在远端节点新增常驻 forward 组件（远端任务由 supervisor 管，端口发布集中在本机 daemon）。
- 不做本机之外的端口发布（`-p` 的"本地"= 执行 CLI 的这台机器）。

## Work 拆解

### Unit 1：`forward` daemon 骨架 + unix socket 控制面
- **改** `cmd/relay/main.go`：新增 `forward` kingpin 子命令组（run/start/status/stop）+ dispatch case。
- **新增** `cmd/relay/forward.go`：`runForward()`（daemon 主循环：加载 config → getRelayBackend → 监听 unix socket → accept 循环）、`daemonStartForward/StopForward/StatusForward`（复用现有 daemon 辅助函数，实例名 `forward`）。
- **验**：`relay forward run`（前台）不崩；`forward status` 有状态；`forward stop` 干净停机。

### Unit 2：socket 控制协议 + 转发循环
- **改** `cmd/relay/forward.go`：unix socket `~/.relay/forward.sock` 监听（启动前 `os.Remove` 清理 stale）；JSON 行式控制消息（op=add/remove/list/status，含 `executor`, `spec`, `bind`, `local_port`, `task_id`）。
- **改** `cmd/relay/portforward.go`：从 `StartPortForwardsForeground` 抽出「单 spec 建立 listener + 双向泵」函数供 daemon 复用（避免两处维护）。
- **改** `cmd/relay/portforward.go`（或 forward.go）：add 检测 local_port 冲突即拒绝+报错（决策 2）；TunnelOpenPortForward 远端断连标失效（决策 5）。
- **验**：单元/端到端——add 后本地端口 listen；连接经 `TunnelOpenPortForward` 达远端 echo；remove 后 listen 关闭；同端口二次 add 被拒。

### Unit 3：`run -d -p` 接入惰性拉起 + 先任务后登记
- **改** `cmd/relay/main.go` `runExec`：detach 分支（`*execDetach && len(pfSpecs)>0`）→ ensure forward daemon running → `ExecStreamWithPorts(detach=true, ports=...)` 启动远端任务并取 TaskID → 逐 spec 经 socket 发 add（带 task_id）→ 打印 TaskID。
- **验**：`relay run --node hub -d -p <lp>:<rp> '<server>'` 返回 TaskID 后 `lsof -iTCP:<lp>` 有监听、`curl` 可达。

### Unit 4：stop 联动清理
- **改** `cmd/relay/main.go` `runStop`：对远端 `stop` 成功后，经 socket 向本机 forward daemon 发 `remove`（按 task_id）精确摘除对应本地转发；remove 失败仅告警不致命（决策 5 失效标注意）。
- **验**：detach 任务 `relay stop <taskid>` 后对应本地端口 listen 关闭；未绑定规则不被误删。

### Unit 5：多 spec 聚合 + 回归 + 重启告警
- **验**：`-p A:B -p C:D` 单进程（`ps` 仅一个 `relay forward run`）；`forward status` 列多条；`forward stop` 后 `forward status` 显示规则已去；模拟 daemon 重启看 status 告警；前台 `run -p` 回归通过。

## Acceptance gates（可验证命令）

```bash
# G1 detach 本地转发可达
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'
lsof -nP -iTCP:18083 -sTCP:LISTEN          # 有监听
curl -s http://127.0.0.1:18083/             # 返回内容

# G2 多 -p 单进程（用不同本地端口避免与 G1 冲突）
./relay run --node hub -d -p 18084:8899 -p 18085:8899 'python3 -m http.server 8899'
ps aux | grep '[r]elay forward run' | wc -l  # == 1（仅 forward daemon）
./relay forward status                        # 列多条规则

# G3 stop 联动（G1 返回的 taskid 记为 T1）
./relay stop T1
lsof -nP -iTCP:18083 -sTCP:LISTEN          # 不再监听（已移除）

# G4 同端口冲突拒绝
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'  # 二次 add 18083 应报错

# G5 前台回归
./relay run --node hub -p 18086:8899 'sleep 5'   # 前台仍监听，无回归
```

## 风险 & 回滚

- **风险 1 双份转发实现**：daemon 与前台各写一套泵 → 抽共用函数（Unit 2）。
- **风险 2 unix socket 残留/权限**：stale socket 致 add 失败 → daemon 监听前 `os.Remove`。
- **风险 3 stop 联动不同步**：远端任务已停但本地 remove 失败 → 规则残留，`forward status` 可见 + 失效标注（决策 5），用户可手工 remove。
- **风险 4 daemon 重启丢规则**：远端任务仍跑但本地端口丢失 → `forward status` 显式告警；恢复须先 `relay stop` 旧任务再重新 `run -d -p`（避免泄漏新任务）。已列入决策 4。
- **回滚**：`git revert` 相关 commits；前台 `-p` 与 detach 路径分离，回滚 detach 改动不影响前台。

## Validation plan

1. `go build ./... && go vet ./...`（改动包）。
2. `go test ./cmd/relay/... ./internal/relay/backend/...`（新增 forward 协议测试，起 in-process hub + echo 服务）。
3. Acceptance gates G1–G5 全过。
4. 回归：现有 tunnel daemon 不受影响（`relay tunnel status` 仍正常）。
