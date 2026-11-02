# Implementation Plan - Multi-Hub Active-Standby Failover (模式一)

- **artifact_contract:** `ce-unified-plan/v1`
- **product_contract_source:** `ce-plan-bootstrap`
- **execution:** `code`

## Goal Capsule

- **Objective:** 消除 Relay 系统中单星型拓扑的单一中转 Hub 单点故障风险 (SPOF)，使执行方 (Agent) 和控制端 (CLI) 具备多 Hub 主备热备与无感漂移重连能力，且支持不同 Hub 具备独立的网络地址、协议 Token 与 HTTP 握手鉴权头 (Headers)。
- **Means:** 采用模式一（主备多星热备，Active-Standby Failover）：
  - 配置层引入 `HubEndpoint` 结构与 `hubs` 对象列表，支持独立配置各 Hub 的 `url`、`token` 与 `headers`，并提供外层全局配置的缺省继承；
  - 客户端层维护 `HubEndpoint` 候选池，拨号与断线重连时按序故障漂移；
  - 漂移时完整切换网络目标、HTTP 握手头与连接 Token，采取粘性连接（Sticky/非抢占），并在新 Hub 上自动恢复自注册握手。
- **Authority Hierarchy:**
  1. 用户明确指定的架构方向：模式一（主备多星热备，零 Hub 间通信耦合）。
  2. 决策闭环：切回策略采用粘性连接（避免来回震荡）；CLI 拓扑探测面向当前激活的 Active Hub；支持多 Hub 独立授权头与 Token。
  3. 兼容性原则：完全向前兼容现有的单一 `url` 配置格式与单 Hub 部署。
- **Stop Conditions:**
  - 当配置多个 Hub 地址时，主 Hub 宕机能够在检测周期内自动将连接平滑漂移至备 Hub。
  - 备 Hub 若具备不同的 Token 或 HTTP Headers，握手与鉴权依然能够成功。
  - 漂移到备 Hub 后，Agent 自动完成重新自注册，CLI 能通过备 Hub 正常发起 `relay exec`、`relay ps`、`relay push` 等。
  - 单 Hub 场景行为、单测与现有端到端测试 100% 保持兼容通过。

---

## Product Contract

### Requirements

- **R1: 配置支持 `hubs` 对象列表与独立鉴权凭据**
  - 在 `backend.config` 中支持 `hubs: []HubEndpoint`，每个条目包含：
    - `url: string`: WebSocket 连接地址（必需）。
    - `token: string`: 该 Hub 的 Relay 认证 Token（可选，缺省继承外层 `token`）。
    - `headers: map[string]string`: 该 Hub 专属的 HTTP 握手请求头（可选，缺省继承外层 `headers` 并支持局部覆盖）。
  - 支持 `urls: []string` 简写形式（自动应用外层全局 `token` 和 `headers`）。
  - 保持对原有 `url: string` 的完全兼容；若 `hubs`、`urls` 与 `url` 同时提供，优先级为 `hubs` > `urls` > `url`。

- **R2: 客户端启动时的有序首选建连**
  - Client / Backend 建立初始连接时，按 `hubs` 列表顺序依次尝试拨号。
  - 单次拨号使用当前 Hub 专属的 `url` 与 `headers` 发起 WebSocket 握手，握手成功后在 `MsgConnect` 中携带其对应的 `token`。
  - 首个握手成功的 Hub 成为当前激活 Hub (`ActiveEndpoint`)；单个 Hub 拨号设置快速超时（默认 3s），避免首个不可达 Hub 导致长时间挂起。

- **R3: 运行时故障快速感知与粘性漂移 (Sticky Failover)**
  - 当与当前 Hub 的连接异常断开或心跳超时（30s 心跳丢失阈值）时，触发 `reconnectLoop`。
  - 重连过程中，若当前 Hub 无法建立连接，轮询尝试下一个可用 Hub，并切换至对应的 `HubEndpoint`（携带其专属鉴权头与 Token）。
  - 一旦成功连接到备 Hub，将当前连接锁定在该 Hub 上（非抢占式），直到该备 Hub 再次断联才触发后续切换。

- **R4: 跨 Hub 漂移后的状态与自注册自动重放**
  - 当 Agent 漂移重连到新 Hub 时，自动重新发送 `MsgConnect` 握手。
  - 如果节点处于 Executor 角色，自动重新下发 `RegisterExecutor` 重新入网，恢复其在集群中的可寻址能力。
  - 工作区事件订阅（`SubscribeEvents`）自动在新 Hub 上重新挂载。

- **R5: CLI 拓扑与当前激活状态可观测**
  - `relay nodes` / `status` 输出当前客户端所连接的 Active Hub 地址及主备状态标签（例如：`Active Hub: wss://hub2 (failover)`）。

### Scope Boundaries

- **In Scope:**
  - `internal/config`: 配置结构支持 `HubEndpoint` 与 `hubs` 列表解析，实现全局继承与向后兼容。
  - `internal/relay/client`: 多 Hub 轮询拨号、鉴权头与 Token 动态切换、故障漂移、重连恢复。
  - `internal/relay/backend`: 自注册重新绑定与事件重连。
  - `cmd/relay`: 状态输出与配置样例更新。
  - 单元测试与多 Hub 漂移 E2E 测试（含跨 Hub 不同鉴权头的测试用例）。
- **Deferred to Follow-Up Work:**
  - 模式二（双挂多活 Active-Active）：同时向多个 Hub 发起连接并去重。
  - 模式三（Hub 联邦 Mesh / 跨地域专线桥接）：Hub 之间建立 BGP-like 路由表同步通道。
- **Outside this product's identity:**
  - Hub 节点间的分布式数据存储（etcd / Raft）——模式一的核心优势即 Hub 保持无状态零耦合。

---

## Planning Contract

### Key Technical Decisions (KTDs)

- **KTD1: 非抢占式故障漂移 (Sticky Failover over Preemptive Switching)**
  - *Context:* 当主 Hub 恢复后，若强制客户端切回，容易在网络不稳定或瞬时波动时引发全网 Agent 震荡（Thundering Herd / Flapping）。
  - *Decision:* 客户端切换到备 Hub 后持续保持。仅在当前连接的 Hub 再次发生断联时，才按照 `hubs` 列表从头再次轮换探测。
- **KTD2: HubEndpoint 独立凭证模型与继承规则 (Independent Endpoint Credentials with Inheritance)**
  - *Context:* 跨机房与灾备场景下，不同 Hub 前置网络（直连 vs 经公网反代）要求的 Token 与 HTTP Header（如 `Cookie`, `Authorization`, `X-Api-Key`）可能截然不同。
  - *Decision:* 引入 `HubEndpoint { URL, Token, Headers }` 统一结构。解析配置时，优先采用当前 Hub 声明的值；未声明的字段回退继承全局外层 `token` 与 `headers`。
- **KTD3: Client 统一抽象与透明漂移 (Transparent Client Multi-Hub)**
  - *Context:* `RelayBackend`、`Watcher`、`CLI` 各处通过 `client.GetOrConnect` 获取客户端。
  - *Decision:* 将多 Hub 轮询、鉴权凭据切换与故障切换逻辑收敛在 `client.Client` 内部，上层保持现有的 `Request` / `Send` / `Transport` 调用接口，对业务调用方透明。
- **KTD4: 快速拨号超时 (Fast Dial Timeout during Failover)**
  - *Context:* 标准 TCP/WebSocket 连接若无超时控制，可能等待 OS 默认的 30s~60s 超时才返回失败，导致漂移延迟过高。
  - *Decision:* 多 Hub 轮询拨号时，单个 Hub 的拨号 context 超时设置为 3 秒。若当前候选 Hub 在 3s 内未完成握手，立即尝试列表中下一个候选。

### Technical Design

```
+-------------------------------------------------------------------------+
|                         Client Multi-Hub Pool                           |
|                                                                         |
| Endpoints: [                                                            |
|   {URL: "ws://hub1...", Token: "tok-1", Headers: {}},                   |
|   {URL: "wss://hub2...", Token: "tok-2", Headers: {"Cookie": "..."}}     |
| ]                                                                       |
|                                                                         |
|  ActiveIndex: 0                                                         |
|         │                                                               |
|         ▼                                                               |
|   dial(Hub1, Header1, timeout=3s) ────► 成功 ────► Active: Hub1         |
|         │ 失败                                                          |
|         ▼                                                               |
|   dial(Hub2, Header2, timeout=3s) ────► 成功 ────► Active: Hub2         |
|                                         (发送 Token2 & 重新自注册)      |
+-------------------------------------------------------------------------+
```

1. **配置模型 (`internal/config/config.go` & `backend/relay.go`)**:
   ```go
   type HubEndpoint struct {
       URL     string            `yaml:"url" mapstructure:"url"`
       Token   string            `yaml:"token" mapstructure:"token"`
       Headers map[string]string `yaml:"headers,omitempty" mapstructure:"headers"`
   }
   ```
   - 解析时，通过 `ParseHubEndpoints(params map[string]interface{}) ([]HubEndpoint, error)` 提取归一化列表：
     - 若提供 `hubs`，解析每个对象，合并全局 `token` 和 `headers`；
     - 若提供 `urls`，对每个 url 生成 HubEndpoint，赋值全局 `token` 和 `headers`；
     - 若仅提供 `url`，生成单元素 HubEndpoint 切片；
     - 校验确保至少存在一个有效 URL。
2. **拨号与重连状态机 (`internal/relay/client/client.go` & `reconnect.go`)**:
   - `Client` 结构体维护：
     - `endpoints []HubEndpoint`: 候选 Hub 端点列表。
     - `activeIdx int`: 当前激活的端点索引。
     - `activeEndpoint HubEndpoint`: 当前激活端点（带读写锁或原子保护）。
   - `dial(ctx)`: 尝试使用 `endpoints[activeIdx]` 拨号：
     - 使用该 endpoint 的 Headers 与 URL 发起 WebSocket 连接。
     - 在 `MsgConnect` 握手报文中携带该 endpoint 的 Token。
   - `establish(ctx)`: 遍历 `endpoints`。单次拨号设置 3s 超时，若失败则切换至下一个索引直至成功。
   - `reconnectLoop`: 当连接断开时，循环尝试 `endpoints`。每次尝试切换到 `(currentIdx + 1) % len(endpoints)`，实现故障平滑漂移。
3. **自注册重发机制 (`internal/relay/backend/relay.go`)**:
   - 在 `establish` 成功之后，触发 `c.onReconnected` 回调。
   - `RelayBackend` 注册该回调，重新发送 `protocol.MsgRegisterExecutor` 与重新拉起 `SubscribeEvents`，保证新 Hub 上具有当前 Agent 的有效路由与事件订阅。

---

## Implementation Units

### U1: 配置层支持 `HubEndpoint` 与 `hubs` 列表解析
- **Files to change:**
  - `internal/config/config.go`
  - `internal/relay/backend/relay.go`
- **Responsibilities:**
  - 定义 `HubEndpoint` 结构体（包含 `URL`, `Token`, `Headers`）。
  - 在 `NewRelayBackend` 与配置解析中支持 `hubs` 列表解析与全局 `token`/`headers` 继承，兼顾兼容 `urls` 与单个 `url`。
  - 增加配置校验与单元测试。
- **Test Scenarios:**
  - `TestConfig_HubEndpoints_SingleURL`: 仅配置 `url`、`token`、`headers` 时，正确归一化为包含 1 个元素的切片。
  - `TestConfig_HubEndpoints_URLsList`: 配置 `urls: [hub1, hub2]` 时，两项均正确继承全局 token 与 headers。
  - `TestConfig_HubEndpoints_FullHubsList`: 配置 `hubs` 列表，包含独立 token 和独立 headers，验证各项隔离与覆盖生效。
  - `TestConfig_HubEndpoints_Inheritance`: 配置 `hubs` 列表部分字段缺省，验证全局默认值的继承逻辑。

### U2: Client 多 Hub 独立凭据拨号与连接池改造
- **Files to change:**
  - `internal/relay/client/client.go`
  - `internal/relay/client/pool.go`
- **Responsibilities:**
  - `Client` 增加 `endpoints []HubEndpoint` 维护能力，导出 `ActiveEndpoint() HubEndpoint`。
  - 构造函数 `New` 与 `GetOrConnect` 支持多 `HubEndpoint` 形式，连接池 key 适配多端点标识。
  - 实现带单节点超时的轮询拨号逻辑：针对当前 endpoint 使用其对应的 URL、Headers 与 Token 发起握手；超时（3s）或失败则尝试下一个 endpoint。
- **Test Scenarios:**
  - `TestClient_Dial_PrimarySuccess`: 主 Hub 正常启动时，直接连上主 Hub，`ActiveEndpoint` 为主 Hub。
  - `TestClient_Dial_FailoverToStandbyWithDifferentAuth`: 主 Hub 离线，备 Hub 启动并要求不同的 Token 与 Header；Client 快速探测主 Hub 失败并在 3s 内使用备用凭据连接上备 Hub。
  - `TestClient_Dial_AllFailed`: 所有 Hub 均无法连接时，返回组合错误信息。

### U3: 运行期故障感知、漂移重连与自注册重放
- **Files to change:**
  - `internal/relay/client/reconnect.go`
  - `internal/relay/client/heartbeat.go`
  - `internal/relay/backend/relay.go`
- **Responsibilities:**
  - 心跳丢包或网络 EOF 触发重连时，轮替尝试备用 Hub 端点。
  - 重连成功后，通过 `onReconnect` 钩子重新执行 Agent 自注册（`RegisterExecutor`）和工作区事件监听。
  - 验证重连后旧的 pending request 正确 fail-closed，新的 request 正常发往新 Hub。
- **Test Scenarios:**
  - `TestClient_Reconnect_Failover`: Client 连着 Hub1，突然终止 Hub1；验证 Client 在退避重连周期内携带备用凭据漂移连接至 Hub2。
  - `TestAgent_Reconnect_ReRegister`: Agent 注册在 Hub1，Hub1 宕机后漂移至 Hub2；验证 Hub2 收到 `MsgRegisterExecutor` 且能向该 Agent 转发 `MsgExec`。

### U4: 拓扑状态与 CLI 可观测性更新
- **Files to change:**
  - `cmd/relay/main.go`
  - `config.example.relay.yaml`
  - `AGENTS.md`
- **Responsibilities:**
  - 更新 `config.example.relay.yaml` 中的注释，展示 `hubs` 完整配置样例与凭证覆盖说明。
  - 在 `relay nodes` / `status` 头部输出当前连接的 Hub 地址（例如 `Active Hub: wss://hub2 (failover)`）。
  - 更新项目知识库 `AGENTS.md`。
- **Test Scenarios:**
  - `TestCLI_Status_ShowsActiveHub`: 运行 `relay status` 输出中包含当前连接的 Active Hub 详细地址。

---

## Verification Contract

- **Quality Gates:**
  1. `go test -v -race ./internal/config/...`: 配置测试通过。
  2. `go test -v -race ./internal/relay/client/...`: 客户端多 Hub 拨号与重连测试通过。
  3. `go test -v -race ./internal/relay/backend/...`: 后端自注册与事件重放测试通过。
  4. `go test -v -race ./...`: 全库回归测试无破坏性故障。
  5. `make build`: 编译构建通过。

---

## Definition of Done

1. 代码全部提交，符合 Go 代码规范与项目约定（中文 Git 提交信息）。
2. 在 `hubs` 配置列表中包含多个 Hub 时，系统具备主备自动探测与故障漂移接管能力，且每个 Hub 可携带独立的 Token 与 Headers。
3. 现有单 `url` 配置用户零改动无感兼容。
4. 全量自动化测试（单元测试与 E2E 场景）通过，零 race condition。
