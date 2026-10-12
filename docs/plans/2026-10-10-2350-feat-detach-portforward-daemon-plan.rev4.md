---
artifact: ce-plan/v1
---

# 后台端口转发 — 实施计划（rev4，daemon=node，无新子命令无新 IPC）

前置：`docs/plans/2026-10-10-2349-feat-detach-portforward-daemon-requirements.md` + rev1~3 评审记录。

## Goal

`relay run -d -p <spec>...` 使本地 `-p` 端口在 CLI 退出后仍可达（远程执行、端口转发到本地）。架构：**本机常驻 daemon 就是 `relay node`（agent 角色）**——`run -d -p` 只负责把「转发规则」交给本机 node，由本机 node 持有本地端口映射表并转发到远端 executor。**不新增任何子命令、不新增独立 forward daemon、不新增 IPC socket**（延续拆 pf 合入 run 的决策，且复用既有 node 生命周期）。

## 架构（核心洞察：daemon 就是 node）

- **本机 node（`relay node` agent 角色）就是常驻承接者**：它已在运行（`relay node start` 拉起，与 `relay tunnel`/`server` 同构），已有到中转的连接、已有生命周期管理（node start/stop/status/restart 全有）。
- **`run -d -p` 只投递规则**：detach + `-p` 时先确保本机 node 在跑（不在则 `relay node start` 拉起），再把 specs 交给本机 node 建本地 listener。CLI 退出后 node 继续持有端口。
- **运行时分布式**：node 内为每个 spec 建一个独立转发单元（goroutine + `TunnelOpenPortForward` 连各自远端 executor）。
- **复用既有命令面**：发布=`relay run -d -p`；查状态=`relay ps -a`（node 侧集成）；停止=`relay stop <taskid>`（node 联动摘规则）；node 本身`relay node status`。
- **无新子命令**：端口是 task_id 的属性（docker：端口是容器属性），全挂在 run/stop/ps 下。

## 关键决策（已拍板）

1. **不新增 forward/pf/隐藏入口**：常驻承接者就是本机 `relay node`（agent）。
2. **`run -d -p` 确保 node 在跑**：detach + `-p` 时，若无本机 node 则 `relay node start` 拉起（node 是既有命令，不是新命令）。
3. **先任务、后登记**：`ExecStreamWithPorts(detach=true)` 经 onChunk 拿 TaskID（relay.go:741 回写），再把 specs 交给本机 node（带 task_id）。任务启动失败则不登记。
4. **同端口冲突=拒绝**：node 对 local_port 冲突返回错误，拒绝该 spec。
5. **stop 联动摘除**：`relay stop <taskid>` 远端成功后，通知本机 node 按 task_id 移除对应转发。
6. **node 重启丢规则显式告警**：node 重启后 `relay ps -a`/node status 提示「规则已丢、远端任务可能仍在」；恢复先 stop 旧任务再重新 run。
7. **孤儿检测**：`TunnelOpenPortForward` 远端断连时标失效并给清理入口。

## Context（代码锚点）

- 断点：`cmd/relay/main.go:1197` `if len(pfSpecs) > 0 && !*execDetach`（detach 跳本地 forwarder）。
- detach 后端：`internal/relay/backend/relay.go:733` `StartTaskWithPorts`，TaskID 回写 `:741`（CLI 经 onChunk 拿）。
- 复用通道：`relay.go:414` `TunnelOpenPortForward`；`internal/relay/client/tunnel.go:326` `Client.TunnelOpenPortForward`。
- 本机 node：`cmd/relay/node.go` `runNode`（hub+agent 角色）；`main.go:232` `node` dispatch（start/stop/status/restart/upgrade/run）。
- 端口转发实现：`cmd/relay/portforward.go:102` `StartPortForwardsForeground`、`:142` `handlePortForwardConn`（抽取复用单元）。

## Scope / Non-goals

### In scope
1. **本机 node 承载转发**：`run -d -p` 将 specs 交给本机 `relay node`（agent），由其在本地建 listener 并转发。node 复用其已有连接。
2. **`run -d -p` 接入**：detach + `-p` → ensure node running → 启动任务拿 TaskID → 投递 specs（带 task_id）→ 打印 TaskID。
3. **`relay stop <taskid>` 联动**：远端 stop 成功 → 通知 node 按 task_id 移除对应本地转发。
4. **`relay ps -a` 增强**：node 侧集成展示本机活跃转发规则（含失效标注）。

### Non-goals
- 不新增用户子命令（forward/pf）；不新增独立 forward daemon；不新增 IPC socket。
- 不做规则跨重启持久化/自动重建（重启后告警 + 手工重新 run）。
- 不改前台 `run -p` 语义（仍 CLI 自持 `StartPortForwardsForeground`）。
- 不在远端节点新增常驻 forward 组件（远端任务由 supervisor 管）。
- 不做本机之外的端口发布（`-p` 的"本地"= 执行 CLI 机器，即本机 node 所在机）。

## Work 拆解

### Unit 1：本机 node 承载转发能力（接收规则 + 转发单元）
- **改** `internal/relay/backend/relay.go` 或 `cmd/relay/node.go`：node（agent）增加「本机 port-forward 持有」逻辑——接收 specs，建本地 listener + 每 spec 一个转发 goroutine（复用 `TunnelOpenPortForward`）。
- **验**：node 收到规则后本地端口 listen；连接连到远端 echo。

### Unit 2：`run -d -p` 接入（ensure node + 先任务后登记）
- **改** `cmd/relay/main.go` `runExec`：`*execDetach && len(pfSpecs)>0` → ensure 本机 node running（不在则 `node start`）→ `ExecStreamWithPorts(detach=true, ports)` 拿 TaskID（onChunk）→ 把 specs 投递给 node（带 task_id）→ 打印 TaskID。
- **验**：`relay run --node hub -d -p <lp>:<rp> '<server>'` 返回 TaskID 后 `lsof -iTCP:<lp>` 有监听、`curl` 可达。

### Unit 3：`relay stop` 联动摘除
- **改** `cmd/relay/main.go` `runStop`：远端 stop 成功 → 通知本机 node 按 task_id 移除对应转发；失败仅告警。
- **验**：`relay stop <taskid>` 后对应本地端口 listen 关闭；未绑定规则不被误删。

### Unit 4：`relay ps -a` 增强 + 多 spec + 回归
- **改** `cmd/relay/main.go` `runPs`：汇总展示本机 node 的活跃转发规则（含失效标注）。
- **验**：`-p A:B -p C:D` 单 node 多转发；`ps -a` 列多条；前台 `run -p` 回归。

## Acceptance gates（可验证命令）

```bash
# G0 node 即承载者
./relay node status                # 本机 node 运行态；未跑则 run -d -p 自动拉起

# G1 detach 本地转发可达
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'
lsof -nP -iTCP:18083 -sTCP:LISTEN
curl -s http://127.0.0.1:18083/

# G2 多 -p 单 node（独立本地端口）
./relay run --node hub -d -p 18084:8899 -p 18085:8899 'python3 -m http.server 8899'
./relay node status                # 仍是本机 node 一个进程
./relay ps -a                      # 列多条转发规则

# G3 stop 联动（G1 任务记 T1）
./relay stop T1
lsof -nP -iTCP:18083 -sTCP:LISTEN  # 不再监听

# G4 同端口冲突拒绝
./relay run --node hub -d -p 18083:8899 'python3 -m http.server 8899'  # 二次绑 18083 被拒

# G5 前台回归
./relay run --node hub -p 18086:8899 'sleep 5'   # 前台仍监听，无回归

# G6 无新增用户子命令
./relay --help 2>&1 | grep -iE 'forward|pf'      # 不得出现 forward/pf 顶层命令
```

## 风险 & 回滚

- **风险 1 本机 node 需先跑**：`run -d -p` 首次需 `node start` 拉起（自动 ensure）；若用户刻意关 node，需提示转发依赖它。
- **风险 2 duplex 转发实现**：node 与前台各写一套 → 抽共用转发单元（Unit 1）。
- **风险 3 stop 联动不同步**：远端已停但本地规则残留 → `ps -a` 可见 + 失效标注，可手工清理。
- **风险 4 node 重启丢规则**：远端任务仍跑但本地端口丢 → `ps -a`/`node status` 明示；恢复先 `relay stop` 旧任务再重跑。
- **回滚**：`git revert`；前台 `-p` 与 detach 路径分离，回滚 detach 不影响前台。

## Validation plan

1. `go build ./... && go vet ./...`。
2. `go test ./cmd/relay/... ./internal/relay/backend/...`（新增转发测试，起 in-process hub + echo）。
3. Acceptance gates G0–G6 全过。
4. 回归：tunnel daemon 不受影响（`relay tunnel status` 正常）；本机 node 正常。
