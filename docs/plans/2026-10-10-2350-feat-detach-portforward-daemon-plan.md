---
artifact: ce-plan/v1
---

# 后台端口转发 daemon 聚合 — 实施计划

前置：`docs/plans/2026-10-10-2349-feat-detach-portforward-daemon-requirements.md`（requirements-only，已过 ready-for-planning）。

## Goal

`relay run -d -p <spec>...` 使本地 `-p` 端口在 CLI 退出后仍可达：本机一个常驻 `forward` daemon 聚合所有本机 port-forward channel（目标 executor 可为 extranet/intranet/hub），`run -d -p` 惰性拉起并登记 spec。前台 `run -p` 语义不变。

## Context（贴代码锚点）

- 断点：`cmd/relay/main.go:1197` `if len(pfSpecs) > 0 && !*execDetach`（detach 跳过本地 forwarder）。
- detach 执行：`internal/relay/backend/relay.go:733` `sup.StartTaskWithPorts(...)`；返回 TaskID 经 `:741`。
- 复用通道：`internal/relay/backend/relay.go:414` `TunnelOpenPortForward`（跳过 network_allow）；`internal/relay/client/tunnel.go:326` `Client.TunnelOpenPortForward`。
- daemon 基建：`internal/daemon/daemon.go` `PidFile/LogFile/Start/Get/Stop`；CLI 侧 `daemonStartArgs`（`main.go:362`）、`daemonStatus`（`:442`）。
- 参照范式：tunnel daemon（`cmd/relay/tunnel.go` `runTunnel` + `main.go:456` `tunnelInstanceName` + `daemonStartTunnel/StopTunnel/StatusTunnel`）。
- 端口 forwarder 实现：`cmd/relay/portforward.go` `StartPortForwardsForeground(:102)` + `handlePortForwardConn(:142)`（本地监听 loop + 双向泵）。
- `relay stop`：`cmd/relay/main.go` `runStop` → 远端 supervisor；本地 daemon 不在远端，需联动触发。

## Scope / Non-goals

### In scope
1. 本机常驻 `forward` daemon（单实例名 `forward`，pid/log 落 `~/.relay/`），子命令 `forward run|start|stop|status|restart`。
2. `forward run` 前台主循环：监听本机 unix domain socket（`~/.relay/forward.sock`），接收 add/remove/list/status 控制消息；每个 spec 建本地 TCP listener + `TunnelOpenPortForward` 远端通道，双向泵（复用 `handlePortForwardConn` 逻辑）。
3. `run -d -p` 惰性拉起：detach 且带 `-p` 时，`daemon.Start`/`daemonStartArgs` 拉起 `forward`（若未运行），再经 unix socket 逐 spec 登记。前台 `run -p` 保持 `StartPortForwardsForeground` 不动。
4. 生命周期跟随任务：规则不持久化；detach 任务 stop（`relay stop <taskid>`）联动移除对应本地转发。

### Non-goals
- 不重建 `relay pf` 子命令。
- 不做规则跨重启持久化/自动重建。
- 不改前台 `run -p` 语义。
- 不在远端节点新增常驻 forward 组件。
- 不做本机之外的端口发布。

## Work 拆解

### Unit 1：`forward` daemon 骨架（进程 + 控制 socket）
- **改** `cmd/relay/main.go`：新增 `forward` kingpin 子命令组 + dispatch case。
- **改** `cmd/relay/portforward.go` 或新增 `cmd/relay/forward.go`：`runForward()`（daemon 主循环）、`daemonStartForward/StopForward/StatusForward`（复用现有 daemon 辅助函数，实例名 `forward`）。
- **验**：`relay forward run`（前台）不崩；`relay forward status` 有状态；`relay forward stop` 干净停机。

### Unit 2：unix socket 控制协议 + 转发循环
- **改** `cmd/relay/forward.go`：unix socket `~/.relay/forward.sock` 监听；JSON 行式控制消息（op=add/remove/list/status，含 `executor`, `spec`, `bind`, `local_port`, `task_id`）。
- **改** `cmd/relay/portforward.go`：从 `StartPortForwardsForeground` 抽出可复用的「单 spec 建立 listener + 转发泵」函数，供 daemon 复用（避免两处维护）。
- **验**：单元/端到端——`add` 后本地端口 listen；连接经 `TunnelOpenPortForward` 达远端 echo 服务；`remove` 后 listen 关闭。

### Unit 3：`run -d -p` 接入惰性拉起 + 登记
- **改** `cmd/relay/main.go` `runExec`：detach 分支（`*execDetach && len(pfSpecs)>0`）→ ensure forward daemon running，逐 spec 登记（经 unix socket），启动任务后返回 TaskID。
- **验**：`relay run --node hub -d -p <lp>:<rp> '<server>'` 返回 TaskID 后 `lsof -iTCP:<lp>` 有监听、`curl` 可达。

### Unit 4：stop 联动清理
- **改** `cmd/relay/main.go` `runStop`：对远端 `stop` 成功后，经 unix socket 向本机 forward daemon 发 `remove`（按 task_id）移除对应本地转发。
- **验**：detach 任务 `relay stop <taskid>` 后对应本地端口 listen 关闭；未绑定的规则不误删其他。

### Unit 5：多 spec 聚合验证 + 回归
- **验**：`-p A:B -p C:D` 单进程（`ps` 仅一个 `relay forward run`）；`forward status` 列两条；前台 `run -p` 回归通过。

## Acceptance gates（可验证命令）

```bash
# G1 detach 本地转发可达
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'
lsof -nP -iTCP:18083 -sTCP:LISTEN          # 有监听
curl -s http://127.0.0.1:18083/             # 返回内容

# G2 多 -p 单进程
./relay run --node hub -d -p 18083:8899 -p 18084:8899 'python3 -m http.server 8899'
ps aux | grep '[r]elay forward run' | wc -l  # == 1（仅 forward daemon）
./relay forward status                        # 列多条规则

# G3 stop 联动
./relay stop <taskid>
lsof -nP -iTCP:18083 -sTCP:LISTEN          # 不再监听（已移除）

# G4 前台回归
./relay run --node hub -p 18085:8899 'sleep 5'   # 前台仍监听
```

## 风险 & 回滚

- **风险 1 双份转发实现**：daemon 与前台各写一套泵 → 抽出共用函数规避（Unit 2）。
- **风险 2 unix socket 残留/权限**：socket 文件 stale 导致 add 失败 → daemon 监听前 `os.Remove` 清理；路径固定 `~/.relay/forward.sock`。
- **风险 3 stop 联动不同步**：远端任务已停但本地 remove 失败 → 规则残留；CLI 侧 stop 成功后再 remove，失败仅告警不致命；后续可在 `forward status` 提供人工清理。
- **回滚**：`git revert` 相关 commits；前台 `-p` 路径与 detach 路径分离，回滚 detach 改动不影响前台。

## Validation plan

1. `go build ./... && go vet ./...`（改动包）。
2. `go test ./cmd/relay/... ./internal/relay/backend/...`（新增 forward 协议测试复用 `testutil` 起 in-process hub + echo 服务，参照 `portforward_test.go`）。
3. Acceptance gates G1–G4 全过。
4. 回归：现有 tunnel daemon 不受影响（`relay tunnel status` 仍正常）。
