# Plan: `relay tunnel status` 表格化 UI 与链路可观测性 (2026-10-10)

针对 `docs/brainstorms/2026-10-10-tunnel-status-ui-requirements.md` 中的需求，将 `relay tunnel status` 从非结构化文本改造为与 `relay nodes` / `relay ps` 一致的对齐表格及 JSON 格式，并打通中转链路状态联动与优雅降级。

---

## 架构与核心逻辑设计

```
[relay tunnel status [--json] [-w <node>]]
                   │
                   ▼
       1. 本地枚举 daemon 实例 (ListInstances("tunnel"))
          - 读取 PID 与存活状态 (Status.Running, Status.Pid)
          - 解析启动参数/监听地址与出口节点 (cmdline 或 log 头部)
                   │
                   ▼
       2. 并发向中转查询全网节点状态 (StatusProbe, 超时 1.5s)
          ├── 成功: 匹配目标节点在线就绪状态 (Ready (xx ms) / Offline)
          └── 失败/超时/断网: 优雅降级 (unknown，不中断命令)
                   │
                   ▼
       3. 格式化输出
          ├── --json: 输出标准 JSON 数组
          └── 默认文本: 输出整洁对齐的 7 列表格 (NAME / STATUS / PID / LISTEN / EGRESS / EGRESS STATUS / LOG)
```

---

## 实施单元 (Implementation Units)

### Unit 1: 隧道实例元数据提取与解析能力
- **目标**：为本地运行中的隧道 daemon 实例解析其绑定的 `LISTEN` 地址和目标 `EGRESS` 节点。
- **文件**：
  - `internal/daemon/daemon.go` 或 `cmd/relay/tunnel_status.go`
- **设计**：
  - 定义 `TunnelInstanceInfo` 结构体：
    ```go
    type TunnelInstanceInfo struct {
        Name        string `json:"name"`
        Status      string `json:"status"` // "running" | "stopped"
        PID         int    `json:"pid"`
        Listen      string `json:"listen"`
        Egress      string `json:"egress"`
        EgressReady bool   `json:"egress_ready"`
        Latency     string `json:"latency,omitempty"`
        LogFile     string `json:"log_file"`
    }
    ```
  - 解析策略（优先级）：
    1. 实例名后半段即为出口节点 ID（`tunnel-<egress>` $\implies$ `Egress: <egress>`）；
    2. 当进程存活时，优先从进程启动命令行（macOS/Linux 下读取 `ps -p <pid> -o args=` 或 `logFile` 首行的 `tunnel: SOCKS5 on <listen> -> egress <node>`）提取准确的 `listen` 地址；
    3. 若停止或未提取到，优雅回退到默认参数或占位符 `-`。

### Unit 2: 中转节点在线状态探针与优雅降级
- **目标**：在 1.5 秒超时内探测出口节点在 Hub 处的注册状态与时延，断网或中转不可达时不报错。
- **文件**：
  - `cmd/relay/tunnel_status.go`
- **设计**：
  - 创建带有 1.5s 短超时的 Context：`ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)`；
  - 尝试复用 `getBackendWithAdaptiveLoopback(cfg)` 并调用 `rb.StatusWithUsage(ctx, false)`；
  - 若调用成功，构建 `map[string]protocol.StatusSegment` 缓存节点的 `Total` 延迟及可用性；
  - 若调用失败或超时，日志静默并把网络探针结果标记为 `nil`（后续渲染为 `unknown`）。

### Unit 3: 表格排版与 JSON 格式化输出
- **目标**：实现类似 `nodes` 和 `ps` 的对齐表格渲染，并支持 `--json` 输出。
- **文件**：
  - `cmd/relay/main.go`
  - `cmd/relay/tunnel_status.go`
- **设计**：
  - 在 `tunnelCmd` 下增加 `--json` 标志：`tunnelJSON = tunnelCmd.Flag("json", "Output as JSON").Bool()`；
  - 改造 `daemonStatusTunnel()`：
    - 无实例时输出：`No active tunnel instances found.`（若 `--json` 则输出 `[]`）；
    - 支持 `-w/--node` 单实例过滤；
    - 文本格式表头：
      `%-16s %-10s %-8s %-18s %-16s %-18s %s`
      `NAME`, `STATUS`, `PID`, `LISTEN`, `EGRESS`, `EGRESS STATUS`, `LOG`；
    - `LOG` 路径自动使用 `~` 缩短家目录路径，提升整齐度。

### Unit 4: 单元测试与端到端实机验证
- **目标**：覆盖有/无实例、中转在线/降级、JSON 输出等测试场景。
- **文件**：
  - `cmd/relay/tunnel_test.go`
- **验证项**：
  1. `make test` 全量通过；
  2. 实机执行 `./relay tunnel status` 查看本地两个真实隧道 `extranet`、`intranet` 输出效果；
  3. 实机执行 `./relay tunnel status --json` 核验 JSON 解析正确性；
  4. 模拟断开或单节点过滤 `./relay tunnel status -w extranet` 测试过滤正确性。

---

## 验证与验收准则
1. 现有隧道守护进程正常识别并渲染为表格；
2. 离线/中转不可达情况下 1.5s 内返回，状态显示 `unknown`，不崩溃不报错；
3. `make test` 保持 100% 通过；
4. Git 提交信息全中文。
