package protocol

import "github.com/user/relay/internal/portfwd"

// ForwardTaskIDMarker 是 detach 执行时 executor 回写任务 ID 的 stdout 行前缀标记。
// 使 CLI 端能精确识别任务 ID,而不与任务自身的 stdout 混淆(任务可能先于 ID 回写打印任意输出)。
const ForwardTaskIDMarker = "__relay_task_id__"

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
	MsgTaskList         MessageType = "task_list"
	MsgTaskStop         MessageType = "task_stop"
	MsgTaskRestart      MessageType = "task_restart"
	MsgTaskLogs         MessageType = "task_logs"
	MsgGitStatus        MessageType = "git_status"
	MsgGitUndo          MessageType = "git_undo"
	MsgPruneExecutors   MessageType = "prune_executors"
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

// RegisterExecutorRequest 向中转注册/注销某 executor(远端执行方)。executor_id 是执行方的
// 节点身份(其 backend.config.executor_id),与中转 watch 存储目录是两回事。
type RegisterExecutorRequest struct {
	ExecutorID string        `json:"executor_id"`
	Action     string        `json:"action"`             // "add" / "remove"
	Version    string        `json:"version,omitempty"`  // 执行方 relay 构建版本(用于 relay version 对比)
	OS         string        `json:"os,omitempty"`       // 执行方操作系统 (runtime.GOOS)
	Arch       string        `json:"arch,omitempty"`     // 执行方 CPU 架构 (runtime.GOARCH)
	Resource   ResourceStats `json:"resource,omitempty"` // 执行方资源基准 (轻量静态规格)
}

// VersionInfo 描述部署中一个 relay 节点的构建信息,用于 `relay version` 跨机对比。
type VersionInfo struct {
	Role       string   `json:"role"`                  // "local"|"transit"|"executor"
	ExecutorID string   `json:"executor_id,omitempty"` // executor 的节点身份;其余角色为空
	Version    string   `json:"version"`
	Commit     string   `json:"commit,omitempty"`
	BuildTime  string   `json:"build_time,omitempty"`
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	Go         string   `json:"go"`
	Args       []string `json:"args,omitempty"`
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
// 各做一次 中→执行方 探针,请求方再补 本地→中转 段。
type StatusRequest struct {
	WithUsage bool `json:"with_usage,omitempty"` // 是否请求实时用量与负载 (重载操作)
}

// ResourceStats 节点资源与负载详情
type ResourceStats struct {
	CPUCores    float64 `json:"cpu_cores,omitempty"`
	CPULoad     float64 `json:"cpu_load,omitempty"`
	MemTotalMB  int64   `json:"mem_total_mb,omitempty"`
	MemUsedMB   int64   `json:"mem_used_mb,omitempty"`
	IsContainer bool    `json:"is_container,omitempty"`
}

// ExecutorHealth 中转对单个已注册执行方的体检结果。
//   - ExecutorID: 执行方的节点身份(executor_id)
//   - Version:    执行方的 relay 构建版本
//   - OS:         执行方操作系统 (runtime.GOOS)
//   - Arch:       执行方 CPU 架构 (runtime.GOARCH)
//   - Seg2:       中→执行方 往返 RTT;执行方离线/探针超时标不可用
//   - Total:      Seg1(本地→中转)+ Seg2,由请求方在本地累加
type ExecutorHealth struct {
	ExecutorID string        `json:"executor_id"`
	Version    string        `json:"version,omitempty"`
	OS         string        `json:"os,omitempty"`
	Arch       string        `json:"arch,omitempty"`
	Resource   ResourceStats `json:"resource,omitempty"`
	Seg2       StatusSegment `json:"seg2"`
	Total      StatusSegment `json:"total,omitempty"`
	// Online 标记执行方是否在线:false 表示它此前注册过但当前断线(见于中转持久化的已知 executor 台账)。
	Online   bool  `json:"online"`
	LastSeen int64 `json:"last_seen,omitempty"` // 最近一次见到的 unix 秒(离线时最后一次心跳/注册时间)
}

// StatusResponse 中转对 MsgStatus 的应答,承接 `relay status/nodes` 的一站式连通性+版本台账。
// status 面向整座部署:请求方不传 watch,中转对**所有**已注册执行方各探针一次。
//   - Seg1:      本地→中转 往返 RTT(由请求方本地 `Ping` 计时,中转不填)
//   - Transit:   中转节点构建信息
//   - Executors: 各在线/已注册执行方的体检;全部离线则为空列表(而非报错)
type StatusResponse struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error,omitempty"`
	Seg1      StatusSegment    `json:"seg1"`
	Transit   VersionInfo      `json:"transit,omitempty"`
	Resource  ResourceStats    `json:"resource,omitempty"` // 中转 Hub 自身资源 (如 K8s Pod 资源)
	Executors []ExecutorHealth `json:"executors,omitempty"`
}

// PushJobRequest push 一个文件直达远端执行方。
// 这是流式 push 的元数据头:文件内容分块走 MsgStreamData(二进制)/MsgStreamEnd。
// ExecutorID 是目标执行方的节点身份。默认(Jobs=true)会触发所在 workspace 的 jobs;
// Jobs=false 时是纯下发传输,执行方把内容原子写到 RelPath(可为绝对路径,用于部署二进制等)
// 即完成,不跑任何 job。
type PushJobRequest struct {
	ExecutorID string `json:"executor_id"`
	RelPath    string `json:"rel_path"` // 落盘目标:相对执行方项目根,或 Jobs=false 时的绝对路径
	Mode       uint32 `json:"mode"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"` // sha256 十六进制,落盘后校验
	StreamID   string `json:"stream_id"`
	Jobs       *bool  `json:"jobs,omitempty"` // nil/true=跑 jobs(默认);false=纯传输
}

// ConfigSyncRequest 流式 config-sync 载荷:把一份新配置(local 端 ExpandEnv 后、
// base64 编码)发到远端执行方,由其校验+原子落盘到自身 config_path。单次应答。
type ConfigSyncRequest struct {
	ExecutorID string `json:"executor_id"`
	Payload    string `json:"payload"` // base64 编码的整份配置内容
}

// ServerUpgradeRequest 服务器自升级请求:客户端把本地构建的新 relay 二进制经流式
// 分块 + sha256 摘要交付给中转,由中转本地自检后可原子换装重启。仅由中转本地处理,
// 绝不转发执行方。二进制内容不放入请求头,经 MsgStreamStart/MsgStreamData/MsgStreamEnd
// 流式帧承载。
type ServerUpgradeRequest struct {
	ExecutorID string `json:"executor_id"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"` // sha256 十六进制,落盘后校验
	StreamID   string `json:"stream_id"`
}

// 隧道消息族(经 executor 出网的 SOCKS5 隧道):
//   - MsgTunnelConnect  本地请求方 → 中转 → executor 的建连请求(携带目标与出口 executor_id)。
//   - MsgTunnelData     双向字节流帧。
//   - MsgTunnelEnd       隧道关闭(任一端断开);另一端触发后关停本地连接。
//
// 建连确认(成功/失败)经 MsgResponse/MsgError 回执(以建连消息 ID 作 RequestID),
// 复用 exec 的转发-回包路径。多 executor 由 TunnelConnectRequest.ExecutorID 选定出口。
type TunnelConnectRequest struct {
	ExecutorID  string `json:"executor_id"`
	Target      string `json:"target"`                 // 目标主机(域名/IP 字面量)
	Port        uint16 `json:"port"`                   // 目标端口
	StreamID    string `json:"stream_id"`              // 隧道 ID(全局唯一)
	PortForward bool   `json:"port_forward,omitempty"` // true = 开发用临时端口映射(跳过 network_allow 白名单)
}

// TunnelData 双向隧道数据帧。兼容旧版本 JSON base64 承载。
type TunnelData struct {
	StreamID string `json:"stream_id"`
	Data     []byte `json:"data,omitempty"`
}

const (
	// TunnelBinaryMagic 是二进制隧道数据帧的前导魔数 (0x54 = 'T')
	TunnelBinaryMagic byte = 0x54
)

// EncodeTunnelBinary 将 streamID 与隧道 payload 编码为紧凑的二进制帧:
// [1B Magic(0x54)] + [1B StreamID 长度 N] + [N 字节 StreamID 字符串] + [Payload 原始字节]
// 避免 JSON 与 Base64 编解码的 33% 膨胀与 CPU 开销。
func EncodeTunnelBinary(streamID string, payload []byte) []byte {
	idLen := len(streamID)
	buf := make([]byte, 2+idLen+len(payload))
	buf[0] = TunnelBinaryMagic
	buf[1] = byte(idLen)
	copy(buf[2:], streamID)
	copy(buf[2+idLen:], payload)
	return buf
}

// DecodeTunnelBinary 解析二进制隧道数据帧。
// 若格式不合法或非隧道魔数,返回 ok = false。
func DecodeTunnelBinary(data []byte) (streamID string, payload []byte, ok bool) {
	if len(data) < 2 || data[0] != TunnelBinaryMagic {
		return "", nil, false
	}
	idLen := int(data[1])
	if len(data) < 2+idLen {
		return "", nil, false
	}
	streamID = string(data[2 : 2+idLen])
	payload = data[2+idLen:]
	return streamID, payload, true
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

// GitSyncPayload 随命令执行一同下发的 Git 增量同步数据包 (Sync-on-Exec)
type GitSyncPayload struct {
	BaseCommit       string `json:"base_commit"`
	Patch            []byte `json:"patch"`
	UntrackedArchive []byte `json:"untracked_archive,omitempty"`
	Force            bool   `json:"force,omitempty"`
}

// ExecRequest 远程执行请求。ExecutorID 指定在哪个执行方上运行。
type ExecRequest struct {
	ExecutorID string          `json:"executor_id"`
	Cmd        string          `json:"cmd"`
	Args       []string        `json:"args,omitempty"`
	Env        []string        `json:"env,omitempty"`
	Cwd        string          `json:"cwd,omitempty"`
	Timeout    int             `json:"timeout"`
	GitSync    *GitSyncPayload `json:"git_sync,omitempty"`
	Detach     bool            `json:"detach,omitempty"`  // true = 后台托管运行并立即返回 TaskID
	Ports      []portfwd.Spec  `json:"ports,omitempty"`   // 关联的端口转发规格(含绑定主机), 如 [{"bind":"0.0.0.0","local_port":8080,...}]
	Restart    string          `json:"restart,omitempty"` // 后台任务的 restart 策略(always|no), 仅 detach 时有意义
}

// TaskInfo 描述跨机传输的任务简报
type TaskInfo struct {
	ID        string         `json:"id"`
	Node      string         `json:"node,omitempty"` // 所属执行方 executor_id
	Cmd       string         `json:"cmd"`
	Cwd       string         `json:"cwd"`
	PID       int            `json:"pid"`
	Ports     []portfwd.Spec `json:"ports,omitempty"` // 端口转发规格(含绑定主机)
	Status    string         `json:"status"`
	ExitCode  int            `json:"exit_code"`
	StartTime int64          `json:"start_time"`
	EndTime   int64          `json:"end_time,omitempty"`
	Restart   string         `json:"restart,omitempty"`    // restart 策略(always|no)
	CreatedAt int64          `json:"created_at,omitempty"` // 首次创建时间(unix 秒),重启不重置
}

// TaskListRequest 查询任务列表
type TaskListRequest struct {
	ExecutorID string `json:"executor_id,omitempty"` // 为空表示查询所有在线节点
	All        bool   `json:"all"`                   // true 包含已退出的历史任务
}

// TaskListResponse 任务列表响应
type TaskListResponse struct {
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	Tasks []TaskInfo `json:"tasks"`
}

// TaskStopRequest 停止任务请求
type TaskStopRequest struct {
	ExecutorID string `json:"executor_id,omitempty"`
	TaskID     string `json:"task_id"`
}

// TaskRestartRequest 重启任务请求:由执行方在原 task id 上按记录的命令/工作目录/端口重跑。
type TaskRestartRequest struct {
	ExecutorID string `json:"executor_id,omitempty"`
	TaskID     string `json:"task_id"`
}

// TaskLogsRequest 获取任务日志请求
type TaskLogsRequest struct {
	ExecutorID string `json:"executor_id,omitempty"`
	TaskID     string `json:"task_id"`
	Follow     bool   `json:"follow"`
	TailLines  int    `json:"tail_lines"`
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

// GitStatusRequest 查询远端工作区 Git 状态
type GitStatusRequest struct {
	ExecutorID   string `json:"executor_id,omitempty"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

// GitSnapshotInfo 影子快照元数据
type GitSnapshotInfo struct {
	Ref       string `json:"ref"`
	Timestamp int64  `json:"timestamp"`
	Message   string `json:"message"`
	CommitSHA string `json:"commit_sha"`
}

// GitStatusResponse 远端工作区 Git 状态返回
type GitStatusResponse struct {
	OK             bool              `json:"ok"`
	Error          string            `json:"error,omitempty"`
	IsGitRepo      bool              `json:"is_git_repo"`
	CurrentBranch  string            `json:"current_branch"`
	BaseCommit     string            `json:"base_commit"`
	IsDirty        bool              `json:"is_dirty"`
	DirtyFiles     []string          `json:"dirty_files,omitempty"`
	UntrackedFiles []string          `json:"untracked_files,omitempty"`
	Snapshots      []GitSnapshotInfo `json:"snapshots,omitempty"`
}

// GitUndoRequest 恢复工作区影子快照请求
type GitUndoRequest struct {
	ExecutorID   string `json:"executor_id,omitempty"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	WorkspaceDir string `json:"workspace_dir,omitempty"`
	TargetRef    string `json:"target_ref,omitempty"` // 为空则恢复最新快照
}

// GitUndoResponse 恢复结果
type GitUndoResponse struct {
	OK          bool   `json:"ok"`
	RestoredRef string `json:"restored_ref,omitempty"`
	SafetyRef   string `json:"safety_ref,omitempty"`
	Error       string `json:"error,omitempty"`
}
