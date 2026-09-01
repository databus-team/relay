# 流式 exec + 多跳透明转发

**日期**: 2026-08-31
**状态**: 实现中
**相关**: 复用并补强现有 `relay` (WebSocket) backend;本地 → 中转 → 远端 watcher

## 背景

用户目前用 fs-mcp「推拉 + 轮询」模式跑 `watch`,无法做到透明转发。仓库内其实已有完整的 `relay` WebSocket backend
(`internal/relay/*`, 6 月实现),已支持事件驱动 watch、push/pull 流式传输、心跳重连。真正缺的:

1. **流式 exec** — 现 `server/client.go handleExec` 是缓冲式的(`sh -c` 跑完才回 `ExecResponse`),无法边跑边输出。
2. **多跳透明转发** — 现 `exec` 由中转 server 本地执行;用户要求中转**只转发、绝不本地执行**。
3. (本轮不做)长驻 agent — 跨命令复用,留后续。

## 架构决策(用户已确认)

- **路由策略**: 远端 watcher 显式向中转注册为本 watch 的 **executor**;中转收到 `exec` 路由给该 executor。若该 watch 无已注册 executor,`exec` 直接**报错**,不做本地兜底。
- **流式语义**: exec 边执行边把 stdout/stderr 增量以帧推回请求方,最后返回 exit code。
- 本轮范围: Phase1 流式 exec + Phase2 多跳转发;Phase3 常驻 agent 留后续。

## 拓扑

```
本地 CLI (relay exec)       中转 (relay server)           远端 (relay watch: relay backend, executor: true)
   │  MsgExec ─────────────? register reqOwner ────?──►  ── 收到 MsgExec,sh -c 流式执行
   │  ◄───────────────── MsgExecOutput/Response ◄────── ── MsgExecOutput(增量) + MsgResponse(exit)
   │           (中转按 reqOwner 原样转发回流)
```

## 协议变更 (internal/relay/protocol/message.go)

新增:
- `MsgExecOutput` — 增量输出帧,携带 `ExecChunk{Seq, Stdout, Data}`, `RequestID` = exec 请求 ID
- `MsgRegisterExecutor` — `RegisterExecutorRequest{WatchID, Action:"add"|"remove"}`
- `ExecChunk`, `RegisterExecutorRequest` 结构体

沿用: `MsgExec`(请求)、`MsgResponse`(最终 `ExecResponse`)、`MsgError`(路由失败)。

## 服务端 (internal/relay/server)

- `server.go`: 新增 `executors map[watchID]clientID` 与 `reqOwner map[reqID]clientID`(+ 锁/读写 helper);断连时清理该 client 的 executor/reqOwner 记录。
- `client.go handleMessage`: 新增 `MsgRegisterExecutor` 与 `MsgExec` 分支;executor 回包(`MsgExecOutput`/`MsgResponse`/`MsgError`)若 `reqID ∈ reqOwner` 则转发回 owner 并(对收尾)清理。
- `handleExec` 重写为**纯转发**:
  1. `GetExecutor(watchID)`;无 → `SendError("no executor registered for watch ...")`
  2. 有 → `SetReqOwner(msg.ID, 请求者ID)`, `SendTo(executorID, msg)`(原样保留 msg.ID)
- 移除本地 `sh -c` 执行分支。

## 客户端 (internal/relay/client)

- `client.go`: 新增 `execStreams map[reqID]chan *Message` 用于请求方流式;`handleMessage` 顶部增加 `routeExecReply`(命中 execStreams 就投递/拦截)。
- 新 API `ExecStream(ctx, cmd, cwd, timeout, onChunk) (*ExecResponse, error)` — 发出 `MsgExec`,循环读 `MsgExecOutput`/`MsgResponse`。
  保持原 `Exec`(缓冲)兼容(经 `pending`,仅收尾 MsgResponse)。
- 执行方(inbound)支持: `SetExecHandler(func(*ExecRequestHandler))`, 客户端读到 inbound `MsgExec` 时若已设 handler 则异步调用并流式回包;未设 handler 回错误。

## 后端 (internal/backend + internal/relay/backend/relay.go)

- `backend.go`: 新增 `ExecChunk{Stdout bool; Data string}` 与可选接口 `ExecStreamBackend{ ExecStream(ctx,cmd,cwd,timeout,on) (int,error) }`。
- `relay.go`: `Exec` 走 `ExecStream` 聚合缓冲区;实现 `ExecStreamBackend`;config 新增 `executor: true` 标志 —— 连接后 `RegisterExecutor`, 并设 inbound exec handler(远端 `sh -c` 流式执行), 供 `relay watch` 扮演执行方。

## CLI (cmd/relay/main.go)

- `runExec`: 若 backend 实现 `ExecStreamBackend`,改走流式并逐文件转发 stdout/stderr,以 exit code 收尾;否则回退 `Exec`。
- `runPush`: relay 后端单文件走 `PushJobSender.Run`,文件直达远端执行方、触发 jobs、输出实时回流;无执行方回退中转暂存。

## 追加:透明 push(2026-09-01)

用户反馈 push 现状「只到中转,watch 拉取,否则要手动 pull」。升级为**透明 push**:

- **协议**: 新增 `MsgPushJob` + `PushJobRequest{WatchID, RelPath, Content(内联 base64)}`;中转按 `reqOwner` 原样转发(同 exec 链路),`MsgExecOutput/Done` 回流 job 输出与 exit code。
- **落地/回退**: 有在线执行方 → 转发给执行方,执行方写文件到 `executor_dir`+rel_path 并跑 jobs;无执行方 → 中转落 `watchDir` 暂存并提示 `[staged ...]`。
- **执行方跑 jobs**: `backend.PushJobHandler` + `PushJobCapable`(relay 实现,进程级共享,避免跨实例被覆盖);`internal/watcher` 暴露 `SetPushJobHandler`(避免 import cycle),由 `cmd/relay.runLocalStoreJobs` 用 `jobrunner.Run` 注入复用本地 job 逻辑。
- **CLI**: `runPush` 单文件走 PushJobSender 流式显示输出。
- **测试**: `TestEndToEnd_PushJob`(直达落盘 + 提示流)、`TestEndToEnd_PushJobNoExecutorFallback`(回退中转暂存)。
- 冒烟:在线 push → 文件落在远端 `executor_dir` 且 job 输出实时回流;离线 push → 落中转。

实施方式仍为纯转发、内联小文件(Push 内容 base64 内嵌),目录 push 维持原路径。

## 测试与文档

- `internal/relay/integration_test.go`: 重写 `TestIntegration_Exec` —— 注册假执行者客户端(模拟收到执行回传);新增流式/无执行方测试。
- 更新 README + config example 说明 `executor: true`、`executor_dir`、透明 exec/push 与报错/回退行为。