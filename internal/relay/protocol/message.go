package protocol

// MessageType 定义所有消息类型
type MessageType string

const (
	MsgConnect     MessageType = "connect"
	MsgConnectAck  MessageType = "connect_ack"
	MsgPing        MessageType = "ping"
	MsgPong        MessageType = "pong"
	MsgFileEvent   MessageType = "file_event"
	MsgEventAck    MessageType = "event_ack"
	MsgPush        MessageType = "push"
	MsgPull        MessageType = "pull"
	MsgList        MessageType = "list"
	MsgExec        MessageType = "exec"
	MsgDelete      MessageType = "delete"
	MsgSubscribe   MessageType = "subscribe"
	MsgResponse    MessageType = "response"
	MsgStreamStart MessageType = "stream_start"
	MsgStreamData  MessageType = "stream_data"
	MsgStreamEnd   MessageType = "stream_end"
	MsgError       MessageType = "error"
	MsgSubscribed  MessageType = "subscribed"
)

// Message 是所有消息的通用包装
type Message struct {
	Type       MessageType `json:"type"`
	ID         string      `json:"id"`
	RequestID  string      `json:"request_id,omitempty"`
	Payload    interface{} `json:"payload,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
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
	StreamID  string `json:"stream_id"`
	Total     int64  `json:"total"`
	Offset    int64  `json:"offset"`
	Remaining int64  `json:"remaining"`
	Digest    string `json:"digest,omitempty"`
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

// EventAck 事件确认
type EventAck struct {
	EventID string `json:"event_id"`
	OK      bool   `json:"ok"`
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
