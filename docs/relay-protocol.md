# 原生 Relay 协议设计方案

## 1. 概述

### 1.1 设计目标

设计一套原生 relay 协议，用于替代现有的 fs-mcp backend，实现：

- **实时事件推送** — 文件变更毫秒级推送给客户端，无需轮询
- **流式大文件传输** — 支持 GB 级文件分片传输，断点续传
- **双向通信** — 客户端可主动 push/pull，服务器可主动推送事件
- **低延迟** — WebSocket 长连接，消除 HTTP 开销

### 1.2 对比现有方案

| 特性 | fs-mcp (当前) | 原生 relay (新) |
|------|--------------|-----------------|
| 通信方式 | HTTP SSE | WebSocket |
| 事件延迟 | 轮询间隔 (≥1s) | 实时 (<50ms) |
| 大文件 | 受限 | 分片流式传输 |
| 认证 | 静态 token | 动态 token + 心跳 |
| 连接模式 | 请求-响应 | 双向长连接 |

---

## 2. 架构设计

### 2.1 系统架构

```
┌─────────────────────┐         ┌─────────────────────┐
│   relay (CLI)       │         │   relay-server      │
│   (客户端 / Watch)  │         │   (服务端 / Storage) │
├─────────────────────┤         ├─────────────────────┤
│ - Watcher          │◀───────▶│ - File Watcher      │
│ - Job Executor     │  WS     │ - File Service      │
│ - CLI Commands     │         │ - Auth Service      │
│ - Relay Client     │         │ - Relay Server      │
└─────────────────────┘         └─────────────────────┘
        │                               │
        │ push/pull/list/exec          │ 文件读写
        ▼                               ▼
   本地目录                      远程存储目录
```

### 2.2 工作流程

#### 模式一：Server Push (文件变更事件)

```
1. relay-server 监听远程目录 (fsnotify)
2. 检测到新文件/修改 → 构造 FileEvent
3. 通过 WebSocket 推送给订阅了该 watch 的 relay (client)
4. relay 匹配 glob 模式 → 触发 job 执行
5. relay 发送 EventAck 确认收到
```

#### 模式二：Client Pull (手动触发)

```
1. 用户执行 relay push/pull/list/exec
2. relay 发送请求到 relay-server
3. server 处理请求 → 返回响应/流式数据
4. relay 处理响应 → 本地执行 job
```

---

## 3. 通信协议

### 3.1 传输层

- **协议**: WebSocket (RFC 6455)
- **URL 格式**: `ws://host:port/relay` 或 `wss://host:port/relay`
- **帧格式**: WebSocket 二进制帧
- **消息编码**: JSON (控制消息) + 二进制流 (文件数据)

### 3.2 消息结构

```go
// 所有消息的通用包装
type Message struct {
    Type    MessageType `json:"type"`     // 消息类型
    ID      string      `json:"id"`       // 消息 ID (UUID)
    RequestID string    `json:"request_id,omitempty"` // 关联的请求 ID (响应时使用)
    Payload interface{} `json:"payload"`  // 消息载荷
}

type MessageType string

const (
    // 连接与认证 (一步完成)
    MsgConnect      MessageType = "connect"
    MsgConnectAck   MessageType = "connect_ack"

    // 心跳
    MsgPing         MessageType = "ping"
    MsgPong         MessageType = "pong"

    // 事件推送 (Server → Client)
    MsgFileEvent    MessageType = "file_event"
    MsgEventAck     MessageType = "event_ack"

    // 请求 (Client → Server)
    MsgPush         MessageType = "push"
    MsgPull         MessageType = "pull"
    MsgList         MessageType = "list"
    MsgExec         MessageType = "exec"
    MsgDelete       MessageType = "delete"
    MsgSubscribe    MessageType = "subscribe"

    // 响应
    MsgResponse     MessageType = "response"
    MsgStreamStart  MessageType = "stream_start"
    MsgStreamData   MessageType = "stream_data"
    MsgStreamEnd    MessageType = "stream_end"
    MsgError        MessageType = "error"

    // 订阅确认
    MsgSubscribed   MessageType = "subscribed"
)
```

### 3.3 连接与认证 (合并为一步)

```
Client                              Server
  │                                    │
  │────── WS Connect ─────────────────▶│
  │                                    │
  │←───── 101 Switching Protocols ─────│
  │                                    │
  │────── {                            │
  │        type: "connect",           │
  │        id: "uuid-xxx",           │
  │        payload: {                 │
  │          client_id: "client-1",  │
  │          token: "secret-token",  │
  │          version: 1,             │
  │          subscribe: ["web-app"]  │
  │        }                           │
  │      } ──────────────────────────▶│
  │                                    │
  │←───── {                            │
  │        type: "connect_ack",       │
  │        id: "uuid-yyy",           │
  │        request_id: "uuid-xxx",  │
  │        payload: {                 │
  │          ok: true,                │
  │          server_id: "srv-1",     │
  │          server_version: 1,      │
  │          watch_dirs: ["web-app"] │
  │        }                           │
  │      } ────────────────────────────│
  │                                    │
  │────── {                            │
  │        type: "pong",              │
  │        payload: {                 │
  │          timestamp: 1234567890   │
  │        }                           │
  │      } ◀───────────────────────────│
```

### 3.4 协议版本协商

```go
// Client 连接时声明支持的协议版本
type ConnectRequest struct {
    ClientID  string   `json:"client_id"`
    Token     string   `json:"token"`
    Version   int      `json:"version"`      // 客户端协议版本
    Subscribe []string `json:"subscribe"`    // 订阅的 watch_id 列表
}

type ConnectResponse struct {
    OK           bool     `json:"ok"`
    Error        string   `json:"error,omitempty"`
    ServerID     string   `json:"server_id"`
    ServerVersion int     `json:"server_version"`
    WatchDirs    []string `json:"watch_dirs"`
}
```

---

## 4. 消息详细定义

### 4.1 文件事件 (Server → Client)

```go
// Server → Client: 文件变更事件
type FileEvent struct {
    EventID   string    `json:"event_id"`    // 事件唯一 ID (UUID)
    WatchID   string    `json:"watch_id"`   // 关联的 watch 配置 ID
    Path      string    `json:"path"`       // 变化的文件的相对路径
    Name      string    `json:"name"`       // 文件名
    Dir       string    `json:"dir"`        // 目录
    Op        FileOp    `json:"op"`         // 操作类型
    Size      int64     `json:"size"`        // 文件大小
    ModTime   int64     `json:"mod_time"`   // 修改时间 (Unix ms)
}

type FileOp string

const (
    OpCreate FileOp = "create"
    OpModify FileOp = "modify"
    OpDelete FileOp = "delete"
    OpRename FileOp = "rename"
)

// Client → Server: 事件确认 (用于去重/幂等)
type EventAck struct {
    EventID string `json:"event_id"`
    OK      bool   `json:"ok"`
}
```

### 4.2 事件去重/幂等机制

- 每个 `FileEvent` 携带唯一 `event_id` (UUID)
- Client 维护 `seenEvents` 集合 (内存缓存，最大 10000 条)
- 收到重复事件时，返回 `EventAck` 但不触发 job
- Server 可选：收到 ack 后记录投递状态，用于统计

### 4.3 文件操作请求

```go
// Client → Server: Push (上传文件)
type PushRequest struct {
    WatchID  string   `json:"watch_id"`
    Path     string   `json:"path"`
    Size     int64    `json:"size"`
    Mode     uint32   `json:"mode"`
    StreamID string   `json:"stream_id"`
}

// Client → Server: Pull (下载文件)
type PullRequest struct {
    WatchID string `json:"watch_id"`
    Path    string `json:"path"`
    Offset  int64  `json:"offset"`   // 断点续传偏移量
}

// Client → Server: List (列出目录)
type ListRequest struct {
    WatchID string `json:"watch_id"`
    Path    string `json:"path"`
    Recurse bool   `json:"recurse"`
    Pattern string `json:"pattern"`
}

// Client → Server: Exec (远程执行命令)
type ExecRequest struct {
    WatchID string   `json:"watch_id"`
    Cmd     string   `json:"cmd"`
    Args    []string `json:"args,omitempty"`
    Env     []string `json:"env,omitempty"`
    Cwd     string   `json:"cwd,omitempty"`
    Timeout int      `json:"timeout"`  // 秒数，默认 30s
}

// Client → Server: Delete (删除文件)
type DeleteRequest struct {
    WatchID string `json:"watch_id"`
    Path    string `json:"path"`
}

// Client → Server: Subscribe (订阅/取消订阅)
type SubscribeRequest struct {
    WatchID string `json:"watch_id"`  // 空 = 订阅所有
    Action  string `json:"action"`    // "add" 或 "remove"
}
```

### 4.4 响应消息

```go
// Server → Client: 通用响应
type Response struct {
    RequestID string      `json:"request_id"`
    OK        bool        `json:"ok"`
    Error     string      `json:"error,omitempty"`
    Payload   interface{} `json:"payload,omitempty"`
}

// 列表响应载荷
type ListResponse struct {
    Entries []FileEntry `json:"entries"`
}

type FileEntry struct {
    Name    string `json:"name"`
    Path    string `json:"path"`
    IsDir   bool   `json:"is_dir"`
    Size    int64  `json:"size"`
    ModTime int64  `json:"mod_time"`
    Mode    uint32 `json:"mode"`
}

// 执行响应载荷
type ExecResponse struct {
    ExitCode int    `json:"exit_code"`
    Stdout   string `json:"stdout"`
    Stderr   string `json:"stderr"`
    Duration int64  `json:"duration_ms"`
}

// 订阅确认
type SubscribedResponse struct {
    WatchID string   `json:"watch_id"`
    OK      bool     `json:"ok"`
    Error   string   `json:"error,omitempty"`
}
```

### 4.5 流式传输

```go
// Server → Client: 流开始
type StreamStart struct {
    StreamID  string `json:"stream_id"`
    Total     int64  `json:"total"`
    Offset    int64  `json:"offset"`     // 实际起始偏移 (断点续传)
    Remaining int64  `json:"remaining"`  // 剩余字节
    Digest    string `json:"digest"`     // SHA256 (可选)
}

// Server → Client: 流数据块
type StreamData struct {
    StreamID string `json:"stream_id"`
    Offset   int64  `json:"offset"`
    Data     []byte `json:"data"`     // 二进制数据
    Chunk    int    `json:"chunk"`   // 块序号
}

// Server → Client: 流结束
type StreamEnd struct {
    StreamID string `json:"stream_id"`
    OK       bool   `json:"ok"`
    Received int64  `json:"received"`
    Digest   string `json:"digest"`
    Error    string `json:"error,omitempty"`
}
```

### 4.6 请求超时

所有请求默认超时 30 秒，可在请求中覆盖：

```go
type RequestOptions struct {
    Timeout time.Duration `json:"timeout"`
}
```

---

## 5. 心跳与健康检测

```go
type Heartbeat struct {
    Timestamp int64 `json:"timestamp"`  // Unix ms
    Latency   int64 `json:"latency"`    // 往返延迟
}

type ConnectionStats struct {
    Latency        int64 `json:"latency_ms"`
    LastPing       int64 `json:"last_ping"`
    ReconnectCount int   `json:"reconnect_count"`
    BytesSent      int64 `json:"bytes_sent"`
    BytesReceived  int64 `json:"bytes_received"`
}
```

- **客户端心跳**: 每 30 秒发送一次 Ping
- **服务端响应**: 立即返回 Pong
- **超时**: 90 秒无响应则断开重连
- **重连策略**: 指数退避 (1s, 2s, 4s, 8s, ... 最大 30s)
- **最大重试次数**: 10 次，之后放弃

---

## 6. 文件传输细节

### 6.1 分片策略

```
┌─────────────────────────────────────────────────────────┐
│                    大文件传输流程                        │
├─────────────────────────────────────────────────────────┤
│ 1. Client 发送 PushRequest (size, stream_id)            │
│ 2. Server 确认: StreamStart (total, offset=0)          │
│ 3. Client 分块发送:                                      │
│    for each 64KB chunk (with flow control):            │
│      StreamData { stream_id, chunk N, data }            │
│ 4. Server 校验并写入磁盘                                 │
│ 5. Server 发送: StreamEnd { received, digest }         │
│ 6. 断点续传: Client 重发时带 offset，Server 跳过已存在部分 │
└─────────────────────────────────────────────────────────┘
```

- **块大小**: 64KB (65536 bytes)
- **校验**: SHA256 端到端校验
- **并发**: 支持多流并发传输 (最大 3 个)

### 6.2 流量控制

```go
type TransferConfig struct {
    ChunkSize   int `json:"chunk_size"`    // 64KB
    MaxInflight int `json:"max_inflight"` // 最大并发块数 (默认 16)
    WindowSize  int `json:"window_size"`  // 滑动窗口 (默认 8)
}
```

- **背压机制**: 当 `inflight chunks >= MaxInflight` 时暂停发送
- **滑动窗口**: 收到 `StreamEnd` 后释放窗口空间，允许继续发送

### 6.3 断点续传

```go
// Pull 带偏移量
type PullRequest struct {
    Path   string `json:"path"`
    Offset int64  `json:"offset"`
}

// Server 从指定偏移开始
type StreamStart struct {
    Offset    int64  `json:"offset"`
    Remaining int64  `json:"remaining"`
}
```

---

## 7. 后端实现

### 7.1 Backend 接口实现

```go
// internal/relay/backend/relay.go

type RelayBackend struct {
    client     *Client
    watchDir   string
    watchID    string
    eventCh    chan FileEvent  // Server 推送的事件
    mu         sync.RWMutex
}

func (b *RelayBackend) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
    resp, err := b.client.Request(ctx, MsgList, ListRequest{
        WatchID: b.watchID,
        Path:    path,
    })
    if err != nil {
        return nil, err
    }
    payload := resp.Payload.(ListResponse)
    return payload.Entries, nil
}

func (b *RelayBackend) Read(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
    // 发送 PullRequest，启动流式接收
    reader := &streamReader{
        client:    b.client,
        streamID:  uuid.New().String(),
        ctx:       ctx,
        buffer:    make(chan []byte, 10),
        errChan:   make(chan error, 1),
    }
    
    // 异步接收流数据
    go func() {
        err := b.client.StartStream(ctx, MsgPull, PullRequest{
            WatchID: b.watchID,
            Path:    path,
            Offset:  offset,
        }, reader.streamID)
        if err != nil {
            reader.errChan <- err
        }
    }()
    
    return reader, nil
}

func (b *RelayBackend) Write(ctx context.Context, path string, r io.Reader) error {
    // 分片上传，实现流式读取和背压
    return b.client.PushStream(ctx, b.watchID, path, r)
}

func (b *RelayBackend) Delete(ctx context.Context, path string) error {
    _, err := b.client.Request(ctx, MsgDelete, DeleteRequest{
        WatchID: b.watchID,
        Path:    path,
    })
    return err
}

func (b *RelayBackend) Exec(ctx context.Context, cmd string, args ...string) (*ExecResult, error) {
    resp, err := b.client.RequestWithTimeout(ctx, MsgExec, ExecRequest{
        WatchID: b.watchID,
        Cmd:     cmd,
        Args:    args,
    }, 30*time.Second)
    if err != nil {
        return nil, err
    }
    payload := resp.Payload.(map[string]interface{})
    return &ExecResult{
        ExitCode: int(payload["exit_code"].(float64)),
        Stdout:   payload["stdout"].(string),
        Stderr:   payload["stderr"].(string),
    }, nil
}

func (b *RelayBackend) SupportsExec() bool { return true }

func (b *RelayBackend) Ping(ctx context.Context) error {
    _, err := b.client.Request(ctx, MsgPing, nil)
    return err
}

func (b *RelayBackend) WatchEvents() <-chan FileEvent {
    return b.eventCh
}

// streamReader 实现 io.ReadCloser，消费 Server 推送的 StreamData
type streamReader struct {
    client    *Client
    streamID  string
    ctx       context.Context
    buffer    chan []byte
    errChan   chan error
    closed    bool
    offset    int64
}

func (r *streamReader) Read(p []byte) (n int, err error) {
    select {
    case data := <-r.buffer:
        n = copy(p, data)
        r.offset += int64(n)
        if len(data) > n {
            // 剩余数据塞回 buffer (简化处理，实际可用环形 buffer)
            r.buffer <- data[n:]
        }
        return n, nil
    case err := <-r.errChan:
        return 0, err
    case <-r.ctx.Done():
        return 0, r.ctx.Err()
    }
}

func (r *streamReader) Close() error {
    r.closed = true
    // 发送流取消信号
    return nil
}
```

### 7.2 Server 实现

```go
// internal/relay/server/server.go

type Server struct {
    addr       string
    watchDirs  map[string]string   // watch_id -> dir
    auth       Authenticator
    wsUpgrader websocket.Upgrader
    watcher    *fsnotify.Watcher   // 跨平台文件监控
    clients    map[string]*Client  // client_id -> Client
    subStore   *SubscriptionStore  // 订阅关系存储
    mu         sync.RWMutex
}

type SubscriptionStore struct {
    // watch_id -> map[client_id]bool
    watchToClients map[string]map[string]bool
    // client_id -> map[watch_id]bool
    clientToWatches map[string]map[string]bool
    mu sync.RWMutex
}

func (s *Server) Serve() error {
    // 1. 启动 WebSocket 服务器
    // 2. 启动 fsnotify 文件监控
    // 3. 监听事件，按订阅关系推送
}

func (s *Server) handleWebSocket(conn *websocket.Conn) {
    client := newClient(conn)
    
    // 读取 connect 消息
    var msg Message
    if err := conn.ReadJSON(&msg); err != nil {
        return
    }
    
    // 验证 token
    req := msg.Payload.(ConnectRequest)
    if !s.auth.Validate(req.Token) {
        conn.WriteJSON(Message{
            Type: MsgConnectAck,
            ID:   uuid.New().String(),
            Payload: ConnectResponse{OK: false, Error: "invalid token"},
        })
        return
    }
    
    // 注册客户端
    s.registerClient(client, req)
    
    // 发送 connect_ack
    conn.WriteJSON(Message{
        Type:       MsgConnectAck,
        ID:         uuid.New().String(),
        RequestID:  msg.ID,
        Payload: ConnectResponse{
            OK:        true,
            ServerID:  s.id,
            WatchDirs: s.getWatchDirs(),
        },
    })
    
    // 启动读循环
    for {
        var msg Message
        if err := conn.ReadJSON(&msg); err != nil {
            break
        }
        s.handleMessage(client, msg)
    }
    
    s.unregisterClient(client)
}

func (s *Server) handleFileEvent(event fsnotify.Event) {
    // 1. 解析 fsnotify.Event -> FileEvent
    fe := parseFileEvent(event)
    
    // 2. 查找订阅了对应 watch 的客户端
    s.subStore.mu.RLock()
    clients := s.subStore.watchToClients[fe.WatchID]
    s.subStore.mu.RUnlock()
    
    // 3. 推送事件
    for clientID := range clients {
        if client, ok := s.clients[clientID]; ok {
            client.Send(Message{
                Type:    MsgFileEvent,
                ID:      uuid.New().String(),
                Payload: fe,
            })
        }
    }
}
```

---

## 8. 配置扩展

### 8.1 Backend 配置

```yaml
backend:
  type: relay
  config:
    url: "wss://server:8443/relay"
    token: "${RELAY_TOKEN}"
    watch_id: "web-app"               # 关联的 watch ID
    # 或 auto 模式: 订阅所有事件，客户端过滤
    auto_subscribe: false
    
    # TLS 配置
    tls:
      enabled: true
      cert_file: "/path/to/cert.pem"
      skip_verify: false              # 生产环境应 false
    
    # 重连配置
    reconnect:
      enabled: true
      max_retries: 10
      initial_delay: 1s
      max_delay: 30s
    
    # 传输配置
    transfer:
      chunk_size: 65536
      max_inflight: 16
      window_size: 8
    
    # 超时配置
    timeout:
      request: 30s
      connect: 10s
      ping: 5s
```

### 8.2 Server 配置

```yaml
relay_server:
  addr: ":8443"
  
  watch:
    - id: web-app
      dir: /var/www/html
    - id: api-files
      dir: /data/api
  
  auth:
    type: token
    tokens:
      - "${RELAY_TOKEN_1}"
      - "${RELAY_TOKEN_2}"
  
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
  
  heartbeat:
    interval: 30s
    timeout: 90s
  
  transfer:
    chunk_size: 65536
    max_concurrent: 3
```

---

## 9. 实现计划

### Phase 1: 基础协议 (1-2 天)

- [ ] 定义消息类型 (protocol/message.go)
- [ ] 实现 JSON 编解码 (protocol/codec.go)
- [ ] 实现 WebSocket Client (client/client.go)
- [ ] 实现 WebSocket Server (server/server.go)
- [ ] 实现连接与认证 (一步完成)
- [ ] 实现 Ping/Pong 心跳
- [ ] 实现重连逻辑 (指数退避)

### Phase 2: 文件操作 (2-3 天)

- [ ] 实现 List 请求/响应
- [ ] 实现 Push 分片上传 + 流量控制
- [ ] 实现 Pull 断点下载
- [ ] 实现 Delete 操作
- [ ] 实现 streamReader (io.ReadCloser)

### Phase 3: 远程执行 (1-2 天)

- [ ] 实现 Exec 请求/响应
- [ ] stdout/stderr 流式返回
- [ ] 超时和取消处理

### Phase 4: 事件推送 (1-2 天)

- [ ] Server 端 fsnotify 监控 (跨平台)
- [ ] 事件订阅模型 (Subscribe)
- [ ] 事件去重 (event_id + seenEvents)
- [ ] Client 端事件处理，触发 job

### Phase 5: 集成与测试 (1 天)

- [ ] 集成到 relay CLI (import backend)
- [ ] 集成到 watcher 流程
- [ ] 压力测试
- [ ] 错误处理和日志

---

## 10. 文件结构

```
internal/
├── relay/
│   ├── protocol/
│   │   ├── message.go      # 消息类型定义
│   │   ├── codec.go       # JSON 编解码
│   │   └── validate.go    # 消息验证
│   ├── client/
│   │   ├── client.go      # WebSocket 客户端
│   │   ├── auth.go        # 连接认证
│   │   ├── reconnect.go   # 重连逻辑
│   │   ├── request.go    # 请求/响应
│   │   └── stream.go      # 流式传输
│   ├── server/
│   │   ├── server.go      # WebSocket 服务器
│   │   ├── handler.go     # 消息处理
│   │   ├── watcher.go     # fsnotify 监控
│   │   ├── auth.go        # 认证中间件
│   │   └── subscribe.go   # 订阅管理
│   └── backend/
│       ├── relay.go       # Backend 实现
│       └── init.go        # 注册 "relay" backend
├── config/
│   └── config.go          # 配置扩展
cmd/relay/
│   ├── server.go          # relay-server 命令
│   └── main.go            # 扩展 import
```

---

## 11. 风险与规避

| 风险 | 规避措施 |
|------|----------|
| WebSocket 连接断开 | 心跳 (30s) + 指数退避重连 + 最大重试 10 次 |
| 大文件传输中断 | 断点续传 + SHA256 校验 + 流量控制 |
| 事件重复/重复执行 | event_id 去重 + seenEvents 集合 |
| 认证 token 泄露 | TLS 加密 + token 轮换机制 |
| 并发冲突 | Server 端文件锁 (os.File.Lock) |
| 内存泄漏 | seenEvents 集合上限 (10000) + 定期清理 |
| 跨平台文件监控 | 使用 fsnotify 库统一抽象 |

---

## 12. 后续扩展

- **压缩传输**: zstd 压缩流式数据
- **多路复用**: 单连接支持多个 watch 并行
- **客户端缓存**: 本地缓存文件元信息
- **审计日志**: 记录所有操作
- **Server 集群**: 多实例部署 + 负载均衡

---

## 13. 依赖

```go
// go.mod 添加
require (
    github.com/gorilla/websocket v1.5.1
    github.com/fsnotify/fsnotify v1.7.0
    github.com/google/uuid v1.5.0
)
```
