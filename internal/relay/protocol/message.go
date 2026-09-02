package protocol

// MessageType 定义所有消息类型
type MessageType string

const (
	MsgConnect          MessageType = "connect"
	MsgConnectAck       MessageType = "connect_ack"
	MsgPing             MessageType = "ping"
	MsgPong             MessageType = "pong"
	MsgFileEvent        MessageType = "file_event"
	MsgPush             MessageType = "push"
	MsgPull             MessageType = "pull"
	MsgList             MessageType = "list"
	MsgExec             MessageType = "exec"
	MsgDelete           MessageType = "delete"
	MsgSubscribe        MessageType = "subscribe"
	MsgResponse         MessageType = "response"
	MsgStreamStart      MessageType = "stream_start"
	MsgStreamData       MessageType = "stream_data"
	MsgStreamEnd        MessageType = "stream_end"
	MsgError            MessageType = "error"
	MsgExecOutput       MessageType = "exec_output"
	MsgRegisterExecutor MessageType = "register_executor"
	MsgPushJob          MessageType = "push_job"
	MsgConfigSync       MessageType = "config_sync"
	MsgVersion          MessageType = "version"
	MsgStatus           MessageType = "status"
	MsgServerUpgrade    MessageType = "server_upgrade"
	MsgTunnelConnect    MessageType = "tunnel_connect"
	MsgTunnelData       MessageType = "tunnel_data"
	MsgTunnelEnd        MessageType = "tunnel_end"
)

// Message 是所有消息的通用包装
type Message struct {
	Type      MessageType `json:"type"`
	ID        string      `json:"id"`
	RequestID string      `json:"request_id,omitempty"`
	Payload   interface{} `json:"payload,omitempty"`
	StreamID  string      `json:"stream_id,omitempty"`
}

// ConnectRequest 客户端连接请求
type ConnectRequest struct {
	ClientID  string   `json:"client_id"`
	Token     string   `json:"token"`
	Version   int      `json:"version"`
	Subscribe []string `json:"subscribe"`
}

// ConnectResponse 服务端连接响应
type ConnectResponse struct {
	OK            bool     `json:"ok"`
	Error         string   `json:"error,omitempty"`
	ServerID      string   `json:"server_id"`
	ServerVersion int      `json:"server_version"`
	WatchDirs     []string `json:"watch_dirs"`
}

// FileOp 文件操作类型
type FileOp string

const (
	OpCreate FileOp = "create"
	OpModify FileOp = "modify"
	OpDelete FileOp = "delete"
	OpRename FileOp = "rename"
)

// FileEvent 文件变更事件
type FileEvent struct {
	EventID string `json:"event_id"`
	WatchID string `json:"watch_id"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Dir     string `json:"dir"`
	Op      FileOp `json:"op"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// ListRequest 列出目录请求
type ListRequest struct {
	WatchID string `json:"watch_id"`
	Path    string `json:"path"`
	Recurse bool   `json:"recurse"`
	Pattern string `json:"pattern"`
}

// ListResponse 列表响应
type ListResponse struct {
	Entries []FileEntry `json:"entries"`
}

// FileEntry 文件条目
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Mode    uint32 `json:"mode"`
}

// Response 通用响应
type Response struct {
	RequestID string      `json:"request_id"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Payload   interface{} `json:"payload,omitempty"`
}

// ExecResponse 执行响应
type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration int64  `json:"duration_ms"`
}

// ExecChunk 单条增量执行输出帧。由执行方发送,RequestID 关联到原始 exec 请求。
type ExecChunk struct {
	Seq    int    `json:"seq"`
	Stdout bool   `json:"stdout"` // true=stdout, false=stderr
	Data   string `json:"data"`
}

// RegisterExecutorRequest 向中转注册/注销某 watch 的 executor(远端执行方)。
type RegisterExecutorRequest struct {
	WatchID string `json:"watch_id"`
	Action  string `json:"action"`            // "add" / "remove"
	Version string `json:"version,omitempty"` // 执行方 relay 构建版本(用于 relay version 对比)
}

// VersionInfo 描述部署中一个 relay 节点的构建信息,用于 `relay version` 跨机对比。
type VersionInfo struct {
	Role      string `json:"role"`               // "local"|"transit"|"executor"
	WatchID   string `json:"watch_id,omitempty"` // executor 归属的 watch;其余角色为空
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	Go        string `json:"go"`
}

// VersionResponse 中转对 MsgVersion 的应答:本机(transit)构建信息 + 各在线执行方构建信息。
type VersionResponse struct {
	OK    bool          `json:"ok"`
	Error string        `json:"error,omitempty"`
	Nodes []VersionInfo `json:"nodes"`
}

// StatusSegment 描述某一跳链路的实测时延(往返 RTT):`local→transit` 与 `transit→executor`
// 各测一次往返并原样记录,`total = seg1 + seg2` 是两段 RTT 之和(读作「总往返」,非单程单跳)。
// LatencyMS 为 nil 表示该段暂不可用(例如远端执行方离线/未注册),Unavailable 给出断点原因,
// 匹配 `relay status` 的降级语义。
type StatusSegment struct {
	LatencyMS   *int64 `json:"latency_ms,omitempty"` // nil = 该段不可用
	Unavailable string `json:"unavailable,omitempty"`
}

// StatusRequest 请求对整座中转做连通性体检。不携带 watch:中转对全部已注册执行方
// 各做一次 中→执行方 探针,请求方再补 本地→中转 段。无 watch 参数,因为 status 面向
// 整座部署(中转 + 所有执行方),而非单个 workspace/executor。
type StatusRequest struct{}

// ExecutorHealth 中转对单个已注册执行方的体检结果。
//   - WatchID: 执行方自注册的 watch_id(executor 身份)
//   - Version: 该执行方的 relay 构建版本
//   - Seg2:    中→执行方 往返 RTT;执行方离线/探针超时标不可用
//   - Total:   Seg1(本地→中转)+ Seg2,由请求方在本地累加
type ExecutorHealth struct {
	WatchID string        `json:"watch_id"`
	Version string        `json:"version,omitempty"`
	Seg2    StatusSegment `json:"seg2"`
	Total   StatusSegment `json:"total,omitempty"`
}

// StatusResponse 中转对 MsgStatus 的应答,承接 `relay status` 的一站式连通性+版本台账。
// status 面向整座部署:请求方不传 watch,中转对**所有**已注册执行方各探针一次。
//   - Seg1:      本地→中转 往返 RTT(由请求方本地 `Ping` 计时,中转不填)
//   - Transit:   中转节点构建信息
//   - Executors: 各在线/已注册执行方的体检;全部离线则为空列表(而非报错)
type StatusResponse struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error,omitempty"`
	Seg1      StatusSegment    `json:"seg1"`
	Transit   VersionInfo      `json:"transit,omitempty"`
	Executors []ExecutorHealth `json:"executors,omitempty"`
}

// PushJobRequest push 一个文件直达远端执行方。
// 这是流式 push 的元数据头:文件内容分块走 MsgStreamData(二进制)/MsgStreamEnd。
// 默认(Jobs=true)会触发所在 workspace 的 jobs;Jobs=false 时是纯下发传输,执行方
// 把内容原子写到 RelPath(可为绝对路径,用于部署二进制等)即完成,不跑任何 job。
type PushJobRequest struct {
	WatchID  string `json:"watch_id"`
	RelPath  string `json:"rel_path"` // 落盘目标:相对执行方项目根,或 Jobs=false 时的绝对路径
	Mode     uint32 `json:"mode"`
	Size     int64  `json:"size"`
	Digest   string `json:"digest"` // sha256 十六进制,落盘后校验
	StreamID string `json:"stream_id"`
	Jobs     *bool  `json:"jobs,omitempty"` // nil/true=跑 jobs(默认);false=纯传输
}

// ConfigSyncRequest 流式 config-sync 载荷:把一份新配置(local 端 ExpandEnv 后、
// base64 编码)发到远端执行方,由其校验+原子落盘到自身 config_path。单次应答。
type ConfigSyncRequest struct {
	WatchID string `json:"watch_id"`
	Payload string `json:"payload"` // base64 编码的整份配置内容
}

// ServerUpgradeRequest 服务器自升级请求:客户端把本地构建的新 relay 二进制经流式
// 分块 + sha256 摘要交付给中转,由中转本地自检后可原子换装重启。仅由中转本地处理,
// 绝不转发执行方。二进制内容不放入请求头,经 MsgStreamStart/MsgStreamData/MsgStreamEnd
// 流式帧承载。
type ServerUpgradeRequest struct {
	WatchID  string `json:"watch_id"`
	Size     int64  `json:"size"`
	Digest   string `json:"digest"` // sha256 十六进制,落盘后校验
	StreamID string `json:"stream_id"`
}

// 隧道消息族(经 executor 出网的 SOCKS5 隧道):
//   - MsgTunnelConnect  本地请求方 → 中转 → executor 的建连请求(携带目标与出口 watch)。
//   - MsgTunnelData     双向字节流帧。
//   - MsgTunnelEnd       隧道关闭(任一端断开);另一端收到后关停本地连接。
//
// 建连确认(成功/失败)经 MsgResponse/MsgError 回执(以建连消息 ID 作 RequestID),
// 复用 exec 的转发-回包路径。多 executor 由 TunnelConnectRequest.WatchID 选定出口。
type TunnelConnectRequest struct {
	WatchID  string `json:"watch_id,omitempty"` // 出口 executor 拥有的 watch;空=按 WatchID 推断
	Target   string `json:"target"`             // 目标主机(域名/IP 字面量)
	Port     uint16 `json:"port"`               // 目标端口
	StreamID string `json:"stream_id"`          // 隧道 ID(全局唯一)
}

// TunnelData 双向隧道字节流帧。Data 经 JSON base64 承载(与 ws 流式帧同风味,不走二进制帧)。
type TunnelData struct {
	StreamID string `json:"stream_id"`
	Data     []byte `json:"data,omitempty"`
}

// TunnelEnd 隧道关闭:任一端断开即发,告知/关停另一端。
type TunnelEnd struct {
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

// PushRequest 上传文件请求
type PushRequest struct {
	WatchID  string `json:"watch_id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Mode     uint32 `json:"mode"`
	StreamID string `json:"stream_id"`
}

// PullRequest 下载文件请求
type PullRequest struct {
	WatchID string `json:"watch_id"`
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
}

// ExecRequest 远程执行请求
type ExecRequest struct {
	WatchID string   `json:"watch_id"`
	Cmd     string   `json:"cmd"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout int      `json:"timeout"`
}

// DeleteRequest 删除文件请求
type DeleteRequest struct {
	WatchID string `json:"watch_id"`
	Path    string `json:"path"`
}

// SubscribeRequest 订阅请求
type SubscribeRequest struct {
	WatchID string `json:"watch_id"`
	Action  string `json:"action"`
}

// SubscribedResponse 订阅确认响应
type SubscribedResponse struct {
	WatchID string `json:"watch_id"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// StreamStart 流开始
type StreamStart struct {
	StreamID   string `json:"stream_id"`
	Total      int64  `json:"total"`
	Offset     int64  `json:"offset"`
	Remaining  int64  `json:"remaining"`
	Digest     string `json:"digest,omitempty"`
	Compressed bool   `json:"compressed,omitempty"`
}

// StreamData 流数据块
type StreamData struct {
	StreamID string `json:"stream_id"`
	Offset   int64  `json:"offset"`
	Data     []byte `json:"data"`
	Chunk    int    `json:"chunk"`
}

// StreamEnd 流结束
type StreamEnd struct {
	StreamID string `json:"stream_id"`
	OK       bool   `json:"ok"`
	Received int64  `json:"received"`
	Digest   string `json:"digest,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Heartbeat 心跳
type Heartbeat struct {
	Timestamp int64 `json:"timestamp"`
	Latency   int64 `json:"latency"`
}

// ConnectionStats 连接统计
type ConnectionStats struct {
	Latency        int64 `json:"latency_ms"`
	LastPing       int64 `json:"last_ping"`
	ReconnectCount int   `json:"reconnect_count"`
	BytesSent      int64 `json:"bytes_sent"`
	BytesReceived  int64 `json:"bytes_received"`
}

const (
	DefaultChunkSize   = 65536
	DefaultMaxInflight = 16
	DefaultWindowSize  = 8
)
