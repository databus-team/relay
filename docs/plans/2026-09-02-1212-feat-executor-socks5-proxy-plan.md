---
title: executor SOCKS5 出网代理 - Plan
type: feat
date: 2026-09-02
topic: executor-socks5-proxy
execution: code
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-brainstorm
---

# executor SOCKS5 出网代理 - Plan

## Goal Capsule

- **Objective:** 给 relay 新增一条「经 executor 出网的 SOCKS5 隧道」：本地任意程序把流量经 SOCKS5 交给本地 CLI，relay 经中转把字节流转发到远端 executor，executor 校验目标地址白名单后发起真实 TCP 连接访问内网目标。让本地能"直连" executor 网络可达的内网域名/IP，同时用目标白名单把这条穿透力收窄到授权目标。
- **Product authority:** 本计划新增一条独立的「隧道」能力，不改动既有命令/文件/config-sync/自升级通道。出口在 executor（中转只转发字节流、绝不落盘或执行任何东西）。协议范围仅 SOCKS5。
- **Open blockers:** 无。四个方向性决策已在对话中确定：通用 SOCKS5 代理 / 出口在 executor / 目标白名单鉴权 / 仅 SOCKS5 范围。

---

## Product Contract

### Summary

在 relay 协议上新增一条「TCP 隧道」消息族，实现本地 → 中转 → 远端 executor 的 SOCKS5 出网代理。本地起一个 SOCKS5 伺服端点（默认 127.0.0.1:xxxx），收到客户端的 SOCKS5 握手与 CONNECT 目标后，把目标地址与后续字节流经 WebSocket 层流式转发到 executor；executor 校验目标地址命中其配置的网络白名单后，向目标发起真实 TCP 连接，并把双向字节流桥回。目标地址白名单在 executor 侧强制，命中才放行；未命中即拒绝，隧道关闭。

### Problem Frame

用户需要「在本地直连一个只有远端 executor 网络能访问的内网域名」。但 relay 现有能力只有「命令」（`exec`，命令在远端以 `sh -c` 跑、回显流回）与「文件」（`push`/`push-job`，落盘触发 jobs），**没有 TCP/HTTP/SOCKS 代理**，不存在「本地直连内网」的通路——`internal/` 里唯一的 `auth/proxy.go` 是登录鉴权用的浏览器 cookie 代理，不是通用隧道。环境约束（远端无公网、三端无 admin、强制经中转）又排除了 frp / ssh -D / Tailscale 等社区方案，故需在 relay 内补齐一条受控的字节流隧道。SOCKS5 是让浏览器/curl/任意程序在本地走 SOCKS5 设代理即可触达内网的最通用协议。

### Requirements

**隧道与出口**

- R1. 本地提供 SOCKS5 伺服端点（监听 `127.0.0.1` 上的用户指定端口），任意 SOCKS5 客户端可连入；中继通过握手解析 CONNECT 目标地址。
- R2. 隧道出口（真实 TCP 连接）必须发生在**远端 executor**：本地只做 SOCKS5 握手与字节流桥；executor 负责向目标发起连接双向搬流。
- R3. 中转服务器对该隧道字节流**只做透明转发，绝不落盘、绝不解析或执行**任何部分——与既有「中转只转发、绝不本地执行」红线一致（同 `exec` 的转发模型）。

**白名单隔离 / 鉴权**

- R4. **目标白名单强制在 executor 侧校验**：executor 收到 CONNECT 目标地址后，必须命中它自身配置的 `network_allow` 策略才放行；未命中一律拒绝并中止。白名单不能只靠本地（本地只解析、不授权）。
- R5. 白名单策略支持按「主机（域名 / IP / CIDR）+ 端口（或端口范围）」声明。**默认拒绝是硬契约而非推荐**：未在 `network_allow` 显式放行的目标一律拒绝；**空的/缺省的 `network_allow` 导致 executor 拒绝一切 `MsgTunnelConnect`**，把配置遗漏时的行为从"未定义/可能放行"钉死为"必拒绝"（fail-closed）。
- R6. 隧道通道的允许使用方由 relay 既有鉴权体系承继（已是 token 硬鉴权），但穿透力由 R4 白名单收窄；隧道开启需服务器显式启用（未显式启用隧道 crate 默认关闭），与既有「升级通道默认关闭」姿态一致。

**安全边界**

- R7. 透明字节流：中继改动不改变既有「token 鉴权 + 受控通道」安全姿态；隧道不授予文件/命令外的新本地执行能力——它只暴露「经 executor 出网访问白名单目标」一种能力。
- R8. 不支持身份权内隧道流无需另外强加密（沿用 WS TLS 即 `wss://`），且不留日志限于必要审计，不全程记录负载内容。

> 注：R8 关于加密/审计属于实现选择边界；负载内容不留存，连接元数据（目标、时长、字节量）可审计。

### Key Decisions

- **通用 SOCKS5 代理** `(user-chosen — chosen over 固定端口映射：要动访问任意目标`任意域名/IP:端口，像 `ssh -D` 一样;成本更高但更通用)` — Governs R1, R2.
- **出口在 executor** `(design — chosen over 由中转出口：守住「中转绝不本执执行」红线；executor 本就是内网调查网络话才是用户想访问的)` — Governs R2, R3.
- **目标白名单鉴权在 executor 侧** `(session-settled: user-directed — chosen over 独立 token 无白名单 / 复用现有 token：白名单把「谁能借 executor 穿内网」收窄到授权目标，救 token 泄漏)(使白名单不被穿越)` — Governs R4–R6.
- **仅 SOCKS5 协议范围** `(user-selected — chosen over SOCKS5+HTTP CONNECT：聚焦、优先交付，SSH/SS 等 HTTPS 隧道可后补)` — Governs R1.
- **SOCKS5 握手取最薄 MVP** `(design — chosen over 全量 RFC1928：仅实现「无认证 + 单 CONNECT」，暂不做 username/password 与 IPv6 协商，控制进入 relay 协议面的复杂度；本地端点认证与 IPv6 留给后续)` — Governs R1, U4.
- **中转失四化设计**：字节流经中转只转发，SOCKS5 握手/CONNECT 解析也不在中转做——转发无解析，白名单在传输端。Governs R3.

### Key Flows

**F1. 隧道建连（本地 → 中转 → executor → 内网）**

- **Trigger:** 本地某个 SOCKS5 客户端（curl/浏览器/其它）连接本地 SOCKS5 端点并发送 CONNECT 请求。
- **Actors:** 本地 SOCKS5 客户端；本地 relay CLI（SOCKS5 伺服）；中转 server（字节流转发）；远端 executor（白名单校验 + 真连接）。
- **Steps:** 本地客户端 `CONNECT host:port` → 本地 server 解析目标 → 经 relay 请求消息把目标地址发给 executor → executor 校验 `network_allow` 白名单 → 命中则向 `host:port` 发起真实 TCP → 回执 OK → 双向字节流经中转桥接 → 任一端断开则关隧道。
- **Outcome:** 合法且被白名单放行的目标 → 本地程序能像连内网直连一样使用该目标；未放行 → CONNECT 阶段即拒绝，不建立任何内网连接。

```mermaid
sequenceDiagram
  participant C as "本地 SOCKS5 客户端"
  participant L as "本地 relay CLI (SOCKS5界面)"
  participant T as "中转 server"
  participant E as "executor(白名单+出网)"
  C->>L: SOCKS5 握手 + CONNECT host:port
  L->>W: 请求(目标 host:port)
  W->>E: 转发请求
  E->>E: 校验白名单(network_allow)
  alt 未命中白名单
    E-->>L: 拒绝
    E-->>C: SOCKS5 握手失败
  else 命中
    E->>E: 发起真实 TCP 连 host:port
    E-->>L: 连成(握手 OK)
    L-->>C: SOCKS5 handshake OK
    C<->>L<->>W<->>E: 双向字节流(经中转只转发)
  end
```

### Acceptance Examples

- AE1. **白名单命中放行** — Given executor 配置允许 `api.internal.com:443`；When 本地 `curl` 经 SOCKS5 CONNECT `api.internal.com:443`；Then 正常建连、数据互通。（Covers R1, R2, R4）
- AE2. **白名单未命中拒绝** — 目标 `10.0.0.20:22` 未命中白名单；CONNECT 失败，不建立内网连接，不产生向目标的任何字节。（R4）
- AE3. **CIDR + 端口范围策略** — 白名单含 `10.0.0.0/8@80,443` 时，`10.1.2.3:443` 放行、`10.9.9.9:22` 拒绝。（R5）
- AE4. **中转失能** — 隧道未过时，字节流在中转上无落盘、无解释、无本地上；中转变更或重启不触发隧道。（R3）
- AE5. **隧道默认关闭** — 服务器未显式开启隧道能力时，隧道请求一律拒绝（与升级通道同姿态）。（R6）
- AE6. **空白名单 fail-closed** — Given executor 未配置 `network_allow` 或为空；When 收到任一 `MsgTunnelConnect`；Then 一律拒绝，不建立任何内网连接，符号沙箱保持关闭。（R5）

### Scope 边界

- 只加「SOCKS5 出网隧道」；**不加** HTTP CONNECT、不做协议端口的任意监听、不提供本地其他协议 client。
- 不开「中途解析/审计负载内容」；中继可记录连接元数据（目标/时长/字节）不存明文负载。
- 不动既有 命令 / 文件 / config-sync / 升级 / 白名单之外目标白名单之外的语义。
- 不分「单连接建多条隧道」的复用复用场景，先做一连接一隧道（见 Deferred）。
- 不做隧道 CLI 之外的分发：本地 SOCKS5 端由本地 `relay` 进程具现（可前台或后台）。

### Success Criteria

- **SC-1.** 一条 `relay tunnel --listen 127.0.0.1:1080` 在 executor 在线、白名单命中情况下，能经 SOCKS5 让本地 `curl --socks5-hostname ` + 内网域访问成功（对质任一被白名单放行的内网 host:port）。
- **SC-2.** 未命中白名单的 CONNECT 被拒、且任何内网目标未受尝试；中继不解释、不落盘隧道负载。
- **SC-3.** 未显式开启隧道的服务器，隧道请求被拒（默认关）。白名单只服务于隧道、不污染文件等其它通道。

### 依赖 / 假设

- 依赖：既有 WS 长连接 + 鉴权体系（`internal/relay/client`、`internal/relay/server`、`internal/relay/protocol`）可承载新消息；复用「中转只转发到 delegation」的转发路径规避连接目标交由 executor。
- 假设：executor 能出网（真连目标）；`wss` 或等效加密承载隧道（公网来源不可靠，留作配置选择）。
- 白名单格式应由 executor 本地配置文件承载（config 扩展）。

### Outstanding Questions

- **Deferred to planning**：新消息型别命名与字段布局（建立隧道 / 目标地址 / 双向字节 / 关断）、白名单配置 schema 与域名解析（解析后在 executor 侧按 IP+端口再校验一次）、SOCKS5 握手在本地完整实现细节（无需 async）、隧道的连接复用/多路复用、中继转发时备份流向的申请。 — 属实现选择，交由 planning 决定。
- **Deferred (后续项)**：HTTP CONNECT 隧道（HTTPS 直连场景）、SOCKS5 用户名/密码认证、连接复用（多路复用）、IPv6/域名解析一致性。

---

## Planning Contract

### Key Technical Decisions

- **KTD1. 新增隧道协议消息族 `MsgTunnelConnect` / `MsgTunnelData` / `MsgTunnelEnd`**：分别对应「建连请求（带目标地址）」「双向字节流」「隧道关闭（任一端断开）」。在 `internal/relay/protocol/message.go` 追加枚举与请求结构，与既有 `PushJobRequest` 同风格。— Governs R1, R2.
- **KTD2. 隧道真实连接只落在 executor**：中转收到隧道消息**不落地、不解析、不本地处理**，建立持久中继转发。无该 watch 的执行者则 fail-fast。隧道中转**不复用**既有 `forwardToExecutor`（它是单方向的请求→执行方路径、且 `MsgResponse` 即清 owner，无法承载隧道的双向持久回程），而是走 KTD8 的双向泵中继。— Governs R2, R3.
- **KTD3. 白名单在 executor 侧强制，新增 `network_allow` 配置**：在 `internal/config/config.go` 定义策略（域名 / IP / CIDR + 端口范围，含 `allow`/默认拒绝）。executor 在 `handleTunnelConnect` 时校验——本地 SOCKS5 只负责握手与字节桥，绝不作为授权边界。— Governs R4–R6.
- **KTD4. 隧道默认关闭**：中转只在配置显式开启隧道时启用；未启用时 `MsgTunnel*` 一律拒绝（与「升级通道默认关闭」姿态一致，复用既有 token 硬鉴权）。— Governs R6.
- **KTD5. 本地 SOCKS5 伺服端点**：新增 `relay tunnel --listen 127.0.0.1:<port>` 子命令，`net.Listen` + SOCKS5 握手（RFC 1928），读取 CONNECT host:port 后把目标发给 executor，双向桥接字节流。— Governs R1.
- **KTD6. 字节流桥与生命周期**：每条隧道 = 一条完整通道（本地 socket ↔ WS ↔ executor 的真实 TCP）；任一端 EOF/异常即双向关闭并 `MsgTunnelEnd`，executor 对齐关闭 TCP 与连接。— Governs R2.
- **KTD7. 并发/体积边界**：隧道通道设置并发上限（单 executor 并发隧道数、本地端并发握手数）与单帧字节上限，并设置每隧道的吞吐/容量上限，超限即拒绝或中止——对照既有流式路径的 `maxSize` 超量中止（`server/client.go` 的 receive-bound 检查），避免 token 持有者无界并发隧道造成内网扫描/资源耗尽（DoS）。— Governs R2, R6.
- **KTD8. 双向隧道中继泵**：中转为每条隧道维护一个持久注册表（`tunnelID → {requesterConn, executorConn}`），在 `MsgTunnelConnect` 时建立、`MsgTunnelEnd`/任一端断连时删除；两条 duplex pump goroutine 分别负责 请求方→执行方 与 执行方→请求方 两个方向的字节泵，双向互不依赖、各自有独立背压。这解决「复用 forwardToExecutor 无法回程」的问题：执行方产生的 `MsgTunnelData` 经该注册表路由回请求方（不经 `maybeRelayExecReply` 的 owner-clearing，也不经单向 `pushRelay` 的 StreamEnd 删除）。— Governs R3.

### High-Level Technical Design

本地 `relay tunnel --listen 127.0.0.1:<port>` 具现一个 SOCKS5 端点；握手后经 WS 发 executor 目标地址；executor 白名单校验通过则 **由 executor 本地**向目标 `net.Dial` 建立真实 TCP（本地仅做握手与字节桥、不建立对目标的连接），双向字节流经中转只转发；未匹配或转发失败则握手返回失败。生命周期随任一端断开而清除。

```mermaid
sequenceDiagram
  participant C as "本地客户端"
  participant L as "本地 relay tunnel"
  participant T as "中转 transit"
  participant E as "executor"
  C->>L: SOCKS5 CONNECT host:port
  L->>T: MsgTunnelConnect
  T->>E: 转发
  E->>E: 校验 network_allow
  alt 命中白名单
    E->>target: 真实 TCP
    E-->>L: 建连 OK
    L-->>C: SOCKS5 OK
    C<->>L<->>T<->>E: MsgTunnelData 双向流
  else 未命中
    E-->>L: 拒绝
    L-->>C: SOCKS5 失败
  end
```

### Output Structure

新增/改动文件概览（沿用 relay 既有分层——本地 relay CLI 的多文件与 executor 复用同一 relay backend）：

```
cmd/relay/main.go                    # 新增 relay tunnel 子命令
internal/relay/protocol/*            # MsgTunnel* 消息族
internal/relay/backend/relay.go      # executor 侧 handleTunnelConnect + 本地 SOCKS5 桥
internal/relay/client/exec.go        # 隧道桥客户端
internal/relay/server/client.go      # 中转隧道透明转发 + 启闭开关
internal/config/config.go            # network_allow 白名单 schema
internal/relay/integration_test.go   # 端到端
docs/relay-protocol.md               # 协议/schema/安全边界
```

### Implementation Units

#### U1. 协议层：`MsgTunnel*` 消息型别与结构

**Goal** 定义隧道建连、双向字节流、关闭三种消息及请求结构，供本地 → executor 双向传输。

**Requirements:** R1, R2

**Files**
- modify `internal/relay/protocol/message.go` — 新增 `MsgTunnelConnect` / `MsgTunnelData` / `MsgTunnelEnd` 与 `TunnelConnectRequest{Target, Port}`。
- modify `internal/relay/protocol/message_test.go` — 表驱动 RoundTrip 覆盖新消息。

**Approach**
- 在 `MessageType` 常量追加 `MsgTunnelConnect`、`MsgTunnelData`、`MsgTunnelEnd`。
- 新增结构 `TunnelConnectRequest{WatchID, Target string, Port uint16, StreamID string}`；字节流用 `MsgTunnelData`（`StreamID` + 二进制 `Data`）。
- 参照 `PushJobRequest` / `ConfigSyncRequest` 的字段与分帧风格（头部请求 + 数据经固定流字节帧）。

**Test scenarios**
- U1-T1 `TunnelConnectRequest` 含 host/port 的 RoundTrip 编码/解码，字段原样保留。
- U1-T2 空/非法字段（空 `Target`）RoundTrip + 校验拒绝。

**Verification**：`go test ./internal/relay/protocol/...`。

#### U2. 中转(transit)：双向隧道中继泵（只转发不落地）

**Files**
- modify `internal/relay/server/client.go` — `handleMessage` switch 增 `MsgTunnelConnect` / `MsgTunnelData` / `MsgTunnelEnd` 三个 case。
- modify `internal/relay/server/server.go` — 增加隧道启闭开关（KTD4，未启用时拒绝 `MsgTunnel*`）与一条**持久隧道注册表 + 双向 pump**（KTD8）。

**Approach**
- 不复用 `forwardToExecutor`（单向、`MsgResponse` 清 owner，承载不了隧道双向回程）。改为 KTD8：`MsgTunnelConnect` 到达 → 校验 to the watch 所注册执行者 → 建立 `tunnelID` 注册项（绑定请求方连接 + 执行方连接），并起两条 duplex pump goroutine（请求方→执行方、执行方→请求方，各自独立背压）。
- 字节流的 `MsgTunnelData` 经该注册表按 `tunnelID` 路由到另一头：请求方向帧→执行方，执行方向帧→请求方，**不经 `maybeRelayExecReply`（owner-clearing 会杀掉隧道），也不经单向 `pushRelay`（StreamEnd 即删）**。
- `MsgTunnelEnd` / 任一端断连 → 拆除该 `tunnelID` 注册项，通知另一端关隧道。
- 隧道启停开关与 KTD4：服务器仅当显式开启时响应；未配置时 `MsgTunnel*` 直接拒绝。
- 字节流在中转上**不解释、不落盘、不聚合**，逐帧原样转发（满足 R3）。

**Test scenarios**
- U2-T1 中转收到 `MsgTunnelConnect` 后建立隧道注册表并转发到该 watch 的执行者；无执行者则回错（fail-fast）。
- U2-T2 隧道未显式开启时，任何 `MsgTunnel*` 被拒（AE5）。
- U2-T3 **回程双向**执行方→请求方的 `MsgTunnelData` 被正确回程到请求方（回程路由可用的关键用例，验证不经单向 `reqOwner` 路径）。
- U2-T4 `MsgTunnelEnd`/断连 → 注册表拆除、另一端关闭，无泄漏。
- U2-T5 中转不落盘：收到后无可写新文件/新增本地字节。

**Verification**：`go test ./internal/relay/...`（server 层 + 转发）。

#### U3. executor：`network_allow` 白名单 + 真实连接

**Files**
- modify `internal/config/config.go` — 新增 `network_allow` 白名单 schema（域名 / IP / CIDR + 端口范围）。
- modify `internal/relay/backend/relay.go` — 新增 `handleTunnelConnect` 白名单校验 + `net.Dial` + 双向 `io.Copy`。

**Approach**
- 白名单策略：宿主（域名 / IP / CIDR）+ 端口（`80,443`、`1-65535`、`any`）。**解析与放行语义钉死**：hostname 条目只是"解析提示"，executor 只解析一次，把解析出的 IP 与 IP/CIDR 白名单逐项比对，命中才放行（纯 hostname 条目必须解析入 IP/CIDR 白名单才通过，不当作"逐名直连"）。
- executor `handleTunnelConnect`：解析 `network_allow` → 命中才 `net.Dial("tcp", <已校验IP字面量>:port, **dial 用已校验的 IP、不用原始 hostname 重新 DNS 解析**，避免 DNS 重绑 TOCTOU) → 回 OK；否则回拒，不向目标发任何字节。
- 生命周期：双向 `io.Copy`；任一方 EOF / 错误 → `MsgTunnelEnd` → 关闭另一方向与连接。

**Requirements:** R2, R4, R5

**Test scenarios**
- U3-T1 命中白名单 → `net.Dial` 成功、可回显（AE1）。
- U3-T2 未命中 → CONNECT 拒绝、不建内网连接（AE2）。
- U3-T3 CIDR + 端口范围（`10.0.0.0/8` + `80`）正确放行 / 拦截（AE3）。
- U3-T4 **dial 用的是已校验的 IP 字面量而非原始 hostname**——断言实际连接目标 = 已放行的解析 IP（DNS 重绑防线）。
- U3-T5 **空/缺省 `network_allow` → 任一 CONNECT 全拒、不建内网连接**（AE6）。

**Verification**：`go test ./internal/relay/backend/...`。

#### U4. 本地：`relay tunnel` SOCKS5 端点

**Files**
- modify `cmd/relay/main.go` — 新增 `relay tunnel --listen` 子命令。
- modify `internal/relay/client/exec.go` / `internal/relay/backend/relay.go` — 本地 SOCKS5 握手与双向桥。

**Approach**
- `relay tunnel --listen 127.0.0.1:1080`：`net.Listen` + SOCKS5 握手（RFC 1928，无认证），读取 target host:port → 发给 executor。
- **本地端点默认且建议仅绑定 loopback（`127.0.0.1`）：若用户指定非 loopback 地址（如 `0.0.0.0`），CLI 必须打印醒目警告，提示该代理对 LAN 无认证开放、会把「经 executor 出内网」的能力暴露给任何能到达该端口的人**。可在交付后续的 SOCKS5 username/password 认证落地前，仅允许 loopback 绑定或强制要求显式认证才可非 loopback。
- 建立后双向字节桥：本地 socket 与 executor TCP 经 WS 中转字节帧互通（与 U3 双向 `io.Copy` 配合）。
- 本地只做握手与转发，不做白名单授权（授权在 executor）。

**Requirements:** R1

**Test scenarios**
- U4-T1 SOCKS5 握手 + CONNECT 到允许目标，回 OK。
- U4-T2 本地 SOCKS5 把字节流桥到 executor 并回显（AE1 反向）。

**Verification**：`go test ./internal/relay/...`；本地 `curl --socks5-hostname 127.0.0.1:1080 http://<允许内网域名>` 冒烟（见 Verification Contract）。

#### U5. 端到端集成：白名单联通 + 安全断言

**Files**
- modify `internal/relay/integration_test.go` — 端到端场景。

**Approach**
- 复用「真实 executor 注册到 exec」的既有集成姿态（`relay_test.go` / `integration_test.go`）：执行方挂载 `network_allow`，本地起隧道，请求到达可访问目标。
- 两个场景：放行目标 → 本地通；未放行目标 → 本地失败且 executor 未向目标发出连接。

**Requirements:** R1–R6

**Test scenarios**
- U5-T1 放行内网目标 → 本地 HTTP 通（AE1）。
- U5-T2 未放行目标 → 本地失败、executor 未连接目标（AE2）。
- U5-T3 端到端「中转不落盘」断言（U2-T3 的集成验证）。

**Verification**：`go test ./internal/relay/...`。

#### U6. 文档

**Files**
- modify `docs/relay-protocol.md` — `MsgTunnel*`、`network_allow` schema、安全表（白名单 executor 强制、隧道默认关闭）。
- modify `README.md` / `AGENTS.md` — `relay tunnel` 用法与白名单示例。

**Approach** 仅在仓库既有文档中同步新行为；改前核对内容避免虚构。

---

## Verification Contract

- 单元 / 集成测试：`make test`（=`go test -v -race ./...`）必须通过。
- 协议层：`go test ./internal/relay/protocol/...`
- 中转层：`go test ./internal/relay/...`
- executor 白名单/连接：`go test ./internal/relay/backend/...`
- 端到端冒烟（建议）：`relay tunnel --listen 127.0.0.1:1080` + `curl --socks5-hostname 127.0.0.1:1080 http://<放行内网域名>` 成功；放行外目标连接失败，executor 不连目标。
- 不改变既有红线：`go test` 通过；确认中转只转发、不本地 execute/落盘隧道字节。

## Definition of Done

- 隧道完整贯通：本地 SOCKS5 握手 → CONNECT 目标 → 中转透明转发 → executor 白名单校验 → 真实 TCP → 双向字节流 → 任一端不接通关闭清理。
- 白名单在 executor 侧强制，未命中不建立内网连接（AE2）。
- 中转只转发、不落盘隧道字节（AE4）。
- 隧道默认关闭、显式开启才可用（AE5）。
- 文档同步（协议、白名单 schema、安全说明）。

各单元 DoD：
- U1：协议 RoundTrip 测试绿。
- U2：转发 + 未开启拒绝 + 不落盘测试绿。
- U3：白名单命中/未命中 + CIDR 测试绿、真实连接成立。
- U4：本地 SOCKS5 伺服 + 桥成功（`curl --socks5-hostname` 通）。
- U5：端到端放行/未放行测试绿。
- U6：文档同步。

---