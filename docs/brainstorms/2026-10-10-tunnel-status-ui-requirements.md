---
date: 2026-10-10
topic: tunnel-status-ui-enhancement
---

# `relay tunnel status` 表格化 UI 与链路可观测性需求规格

## Problem Frame

当前 `relay tunnel status` 采用非结构化的原始键值对纯文本输出：
```text
tunnel-extranet: running (pid 26993)
log: /Users/chentt/.relay/tunnel-extranet.log
tunnel-intranet: running (pid 27063)
log: /Users/chentt/.relay/tunnel-intranet.log
```
这导致两个关键体验与排障痛点：
1. **排版混乱**：无法快速一览全部活跃隧道的端口映射与进程号；
2. **链路故障盲区**：SOCKS5 隧道是「本地进程 $\to$ 中转 Transit $\to$ 远端出口节点」的三段式链路。当远端 Windows/Linux 节点断网、关机或脱机时，本地隧道守护进程仍然处于 `running`，导致用户发起代理访问持续超时报错，却无法通过 `tunnel status` 发现出口节点早已失效。

用户需要一种类似 `relay nodes` / `relay ps` 的整洁表格化界面，能够一览本地隧道端口、目标出口节点及其链路在线就绪状态。

---

## Requirements

### 1. 表格化视图输出（Table Output）
- **R1.1 对齐表格格式**：当运行 `relay tunnel status` 且存在隧道实例时，输出对齐的纯文本表格（列宽自适应或规范宽度）。
- **R1.2 默认列结构**：表格包含以下 7 列：
  - `NAME`：隧道实例标识（如 `extranet` 或 `intranet`，去除冗余的 `tunnel-` 前缀）；
  - `STATUS`：本地守护进程状态（`running` 或 `stopped`）；
  - `PID`：进程 PID（运行中显示具体 PID，已停止显示 `-`）；
  - `LISTEN`：本地 SOCKS5 监听地址端口（例如 `127.0.0.1:1081`）；
  - `EGRESS`：绑定的出口节点标识（即目标 executor_id，例如 `extranet`）；
  - `EGRESS STATUS`：出口节点的全网在线状态与时延（例如 `Ready (24 ms)` 或 `Offline`）；
  - `LOG`：日志文件路径（以波浪号 `~` 开头缩短家目录路径，提升可读性）。
- **R1.3 无实例提示**：当本地未启动任何隧道实例时，输出友好提示：`No active tunnel instances found.`，退出码为 0。

### 2. 远端出口就绪状态联动与优雅降级（Egress Readiness & Degradation）
- **R2.1 出口在线探测**：执行 `relay tunnel status` 时，若中转可达，并发向中转查询全网节点在线台账（复用 `relay nodes` 的状态查询机制），获取目标节点是否 Ready 及其端到端时延（RTT）。
- **R2.2 离线/失联明确标注**：若目标出口节点未在中转注册或处于断开状态，`EGRESS STATUS` 明确显示 `Offline`。
- **R2.3 优雅降级（Graceful Degradation）**：如果当前处于离线环境、中转未启动或探测超时（设置 1.5s 快速超时限制），命令绝不失败退出，而是将 `EGRESS STATUS` 降级显示为 `unknown`，完整输出本地守护进程表格。

### 3. 本地监听端口与参数探测
- **R3.1 监听地址解析**：本地守护进程启动时使用命令行参数 `--listen <addr>`。`tunnel status` 优先从当前进程命令行（或 PID 关联的监听端口）解析出实际绑定的 `LISTEN` 地址；若无法解析则展示默认配置或参数占位。

### 4. 机器可读输出（JSON Mode）
- **R4.1 `--json` 标志**：支持 `relay tunnel status --json`，输出标准的 JSON 数组。
- **R4.2 结构化字段**：每个元素包含：
  - `name`: string
  - `status`: string ("running" | "stopped")
  - `pid`: int (0 if stopped)
  - `listen`: string
  - `egress`: string
  - `egress_ready`: bool
  - `latency_ms`: *int64 (nullable)
  - `log_file`: string

### 5. 单实例过滤兼容
- **R5.1 `-w/--node/--executor` 过滤**：当用户执行 `relay tunnel status -w extranet` 时，仅展示该指定实例的一行（或单个 JSON 对象），与现状保持行为一致。

---

## Success Criteria

1. 执行 `relay tunnel status` 输出对齐表格，格式风格与 `relay nodes`、`relay ps` 保持统一：
   ```text
   NAME       STATUS   PID     LISTEN          EGRESS    EGRESS STATUS   LOG
   extranet   running  26993   127.0.0.1:1081  extranet  Ready (24 ms)   ~/.relay/tunnel-extranet.log
   intranet   running  27063   127.0.0.1:1080  intranet  Ready (28 ms)   ~/.relay/tunnel-intranet.log
   ```
2. 当断开中转网络时执行 `relay tunnel status`，命令依然在 1.5s 内秒级返回，`EGRESS STATUS` 降级为 `unknown`，本地进程信息不丢失。
3. 执行 `relay tunnel status --json` 输出合法 JSON，可直接供 jq 或脚本处理。
4. 无实例时输出 `No active tunnel instances found.`，不产生 panic 或空指针。

---

## Scope Boundaries

- **不做交互式 TUI**：保持 CLI 命令行管道输出，不引入 ncurses 等重量级交互界面。
- **不修改底层隧道传输协议**：仅改造客户端状态汇总与展示层，不触动 WebSocket 隧道二进程流转协议。
- **不增加对系统的外部侵入性依赖**：进程及端口探测仅依赖 Go 标准库或现有 `internal/daemon`，不依赖外部 `lsof` / `netstat` 二进制命令。
