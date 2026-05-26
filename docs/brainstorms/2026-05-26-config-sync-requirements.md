---
date: 2026-05-26
topic: config-sync
---

# Config Sync & Hot Reload

## Problem Frame
需要在不重启 watch 进程的情况下热更新配置文件（config.yaml），并确保失败时可回滚。当前配置只在启动时加载，缺少安全的在线更新机制。

```mermaid
flowchart TB
  A[User runs relay sync] --> B[CLI sends built-in reload command via exec channel]
  B --> C[Watcher receives command]
  C --> D{Validate new config}
  D -- invalid --> E[Return error; keep current config]
  D -- valid --> F[Backup current config (single)]
  F --> G[Apply config; reload next cycle]
  G --> H[Return success result]
```

## Requirements

**Sync Command Behavior**
- R1. 提供显式的同步命令（如 `sync`），使用现有 exec 通道把新配置内容发送到远端 watcher，并触发热更新；不要求用户指定远端配置路径。
- R2. `sync` 必须等待 watcher 返回成功/失败结果，并在 CLI 输出中体现错误原因。
- R3. 同步作用范围为全局配置（影响所有 watch）。

**Watcher Reload Behavior**
- R4. watcher 识别并拦截内置的“配置同步”命令，在进程内重载配置，不退出进程。
- R5. 新配置不应打断正在执行的 jobs，改动从下一个周期生效。

**Safety & Backup**
- R6. 应用新配置前必须校验配置格式；失败时保持当前配置不变并返回错误。
- R7. 成功更新前创建单一备份（覆盖式），便于快速回滚。

## Success Criteria
- 运行 `sync` 后，watcher 返回明确的成功/失败结果。
- 成功时，watcher 在不中断进程的情况下应用新配置，并从下一个周期生效。
- 失败时，旧配置仍然有效且有错误提示。
- 更新成功时，会产生一份可回滚的单一备份。

## Scope Boundaries
- 不做按 watch ID 的单独配置覆盖。
- 不引入 UI 或额外的管理服务。
- 不改变现有任务执行语义（仅影响配置加载方式）。

## Key Decisions
- 采用显式 `sync` 命令，并复用 exec 通道发送配置内容与结果回传。
- 配置更新不打断正在执行的 jobs，改动从下一个周期生效。
- 备份策略为“仅保留上一次备份（覆盖式）”。

## Dependencies / Assumptions
- watch 进程启动时通过 `-c` 指定了本地配置路径，并以此作为热更新的落地位置。
- exec 通道可用且稳定（用于传递配置内容与结果）。

## Outstanding Questions

### Deferred to Planning
- [Needs research] 配置内容的传输与落盘是否需要大小限制或分片机制。

## Next Steps
-> /ce:plan for structured implementation planning
