package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/version"
)

// Client 服务端客户端连接
type Client struct {
	id       string
	conn     *websocket.Conn
	server   *Server
	closeCh  chan struct{}
	sendMu   sync.Mutex
	streams  map[string]*ReceiveStream
	streamMu sync.RWMutex
}

// NewClient 创建客户端
func NewClient(conn *websocket.Conn, server *Server) *Client {
	return &Client{
		conn:    conn,
		server:  server,
		closeCh: make(chan struct{}),
		streams: make(map[string]*ReceiveStream),
	}
}

// SetID 设置客户端 ID
func (c *Client) SetID(id string) { c.id = id }

// ID 获取客户端 ID
func (c *Client) ID() string { return c.id }

// CloseCh 返回关闭通道
func (c *Client) CloseCh() <-chan struct{} { return c.closeCh }

// Run 运行客户端处理循环
func (c *Client) Run() {
	go c.writeLoop()
	c.readLoop()
}

// readLoop 读循环
func (c *Client) readLoop() {
	defer close(c.closeCh)
	defer c.conn.Close()

	for {
		msgType, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		if msgType == websocket.BinaryMessage {
			continue
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			c.SendError("", "invalid message")
			continue
		}

		if msg.Type == protocol.MsgStreamData {
			binType, binData, err := c.conn.ReadMessage()
			if err == nil && binType == websocket.BinaryMessage {
				if payload, ok := msg.Payload.(map[string]interface{}); ok {
					payload["data"] = binData
				}
			}
		}

		c.handleMessage(msg)
	}
}

// writeLoop 写循环
func (c *Client) writeLoop() {
	// 目前不需要额外的写循环，消息直接在 handleMessage 中发送
}

// handleMessage 处理消息
func (c *Client) handleMessage(msg protocol.Message) {
	// 执行方回包(被转发 exec 的输出帧)按 reqOwner 转回请求方
	if c.maybeRelayExecReply(msg) {
		return
	}

	// 被转发给执行方的流式 push 内容帧原样透传,不进入本地流处理。
	if c.maybeRelayPushStream(&msg) {
		return
	}

	switch msg.Type {
	case protocol.MsgPing:
		c.Send(protocol.Message{Type: protocol.MsgPong, ID: uuid.New().String(), RequestID: msg.ID})

	case protocol.MsgList:
		c.handleList(msg)

	case protocol.MsgPull:
		c.handlePull(msg)

	case protocol.MsgPush:
		c.handlePush(msg)

	case protocol.MsgDelete:
		c.handleDelete(msg)

	case protocol.MsgExec:
		c.handleExec(msg)

	case protocol.MsgRegisterExecutor:
		c.handleRegisterExecutor(msg)

	case protocol.MsgVersion:
		c.handleVersion(msg)

	case protocol.MsgStatus:
		c.handleStatus(msg)

	case protocol.MsgPushJob:
		c.handlePushJob(msg)

	case protocol.MsgConfigSync:
		c.handleConfigSync(msg)

	case protocol.MsgServerUpgrade:
		c.handleServerUpgrade(msg)

	case protocol.MsgSubscribe:
		c.handleSubscribe(msg)

	case protocol.MsgPong:
		// 执行方的探针回包到达其自身连接;按 RequestID 交给 `pending` 中等待的 status 探活方。
		if c.maybeResolvePending(msg) {
			return
		}

	case protocol.MsgStreamData:
		c.handleStreamData(msg)

	case protocol.MsgStreamEnd:
		c.handleStreamEnd(msg)
	}
}

// handleList 处理列表请求
func (c *Client) handleList(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	path := toString(payload["path"])

	dir, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	fullPath := safePath(dir, path)
	if fullPath == "" {
		c.SendError(msg.ID, "path traversal detected")
		return
	}

	entries, err := readDir(fullPath)
	if err != nil {
		c.SendError(msg.ID, err.Error())
		return
	}

	c.SendResponse(msg.ID, protocol.ListResponse{Entries: entries})
}

// handleDelete 处理删除请求
func (c *Client) handleDelete(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	path := toString(payload["path"])

	dir, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	fullPath := safePath(dir, path)
	if fullPath == "" {
		c.SendError(msg.ID, "path traversal detected")
		return
	}

	if err := os.RemoveAll(fullPath); err != nil {
		c.SendError(msg.ID, err.Error())
		return
	}

	c.SendResponse(msg.ID, nil)
}

// forwardToExecutor 把到达中转的请求原样透明转发给该 watch 的已注册执行方。
// 中转不本地执行:校验 watch → 找执行方 → 记录请求方归属(供返回值流帧原路返回)→
// 原样转发(保留 msg.ID 作关联)。无执行方或转发失败时回执错误。
func (c *Client) forwardToExecutor(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])

	if _, ok := c.server.GetWatchDir(watchID); !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	executorID, ok := c.server.GetExecutor(watchID)
	if !ok {
		c.SendError(msg.ID, "no executor registered for watch '"+watchID+"'")
		return
	}

	c.server.SetReqOwner(msg.ID, c.id)
	if err := c.server.SendTo(executorID, msg); err != nil {
		c.server.ClearReqOwner(msg.ID)
		c.SendError(msg.ID, "executor unavailable: "+err.Error())
	}
}

// handleExec 处理执行请求。纯透明转发给该 watch 的已注册执行方。
func (c *Client) handleExec(msg protocol.Message) {
	c.forwardToExecutor(msg)
}

// handleConfigSync 处理 config-sync 请求。与 handleExec 同构(校验 watch → 找执行方 →
// 记录请求方 → 原样转发);config 必须只在真执行方上落盘,无执行方时直接 fail-fast,不透传暂存。
func (c *Client) handleConfigSync(msg protocol.Message) {
	c.forwardToExecutor(msg)
}

// handleServerUpgrade 处理「服务器自升级」消息,由中转本地处理,绝不转发执行方。
// 鉴权硬约束(KTD5/R2):仅当服务器显式配置了 token 时升级通道才可用;未配置 token 时
// 通道默认关闭——与既有「无 token 即放行」的文件交换姿态显式区分。自检通过后先回执
// 成功 ACK,再进入 U3 的换装(停旧 → .prev 备份 → 替换 → 重启)。
func (c *Client) handleServerUpgrade(msg protocol.Message) {
	if len(c.server.auth.Tokens) == 0 {
		c.SendError(msg.ID, "server upgrade: token auth not configured; channel disabled")
		return
	}

	var req protocol.ServerUpgradeRequest
	if b, err := json.Marshal(msg.Payload); err == nil {
		_ = json.Unmarshal(b, &req)
	}
	streamID := req.StreamID
	if streamID == "" {
		c.SendError(msg.ID, "server upgrade: missing stream_id")
		return
	}

	// 落盘路径完全由服务端生成(随机 uuid),绝不使用客户端提供的路径/字段;并 O_EXCL
	// 独占创建,防覆盖/防路径穿越。内容被子继承流式接收落盘,收尾(rename 完成)再校验+自检。
	tmpBin := filepath.Join(os.TempDir(), "relay-upgrade-"+uuid.New().String()+".bin")
	partPath := tmpBin + ".incoming"
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		c.SendError(msg.ID, "server upgrade: create temp: "+err.Error())
		return
	}
	f.Close()

	reqID := msg.ID
	c.streamMu.Lock()
	c.streams[streamID] = &ReceiveStream{
		streamID: streamID,
		path:     tmpBin,
		tmpPath:  partPath,
		maxSize:  req.Size, // 升级流上限:超量即中止,防 /tmp 被无限写满
		onDone: func() {
			c.finishServerUpgrade(reqID, tmpBin, req.Digest, req.Size)
		},
	}
	c.streamMu.Unlock()
}

// finishServerUpgrade 流式收盘后在 onDone 上执行:sha256 摘要比对 → 本地自检 → 自检通过
// 则先回执成功 ACK(此时换装尚未开始),随后由 swapServerUpgrade(U3) 接手停旧/备份/换装/重启。
func (c *Client) finishServerUpgrade(reqID, tmpBin, expectedDigest string, expectedSize int64) {
	// 0) 尺寸校验:落盘大小必须与请求声明一致(拦截截断或声明不符的残件)。
	if info, err := os.Stat(tmpBin); err == nil && expectedSize > 0 && info.Size() != expectedSize {
		os.Remove(tmpBin)
		c.SendError(reqID, fmt.Sprintf("server upgrade: size mismatch: received %d != declared %d", info.Size(), expectedSize))
		return
	}
	// 1) 摘要与请求声明比对;不一致即中止,不触碰现行二进制。
	if err := verifyFileDigest(tmpBin, expectedDigest); err != nil {
		os.Remove(tmpBin)
		c.SendError(reqID, "server upgrade: digest mismatch: "+err.Error())
		return
	}
	// 1.5) 落盘二进制需可扩展为做子进程自检;置 0755(不影响真相校验)。
	_ = os.Chmod(tmpBin, 0o755)
	// 2) 自检:子进程 `version` 探测是否可启动(超时 10s)。语义上限为防损坏/防不可启动。
	if err := selfCheckBinary(context.Background(), tmpBin); err != nil {
		os.Remove(tmpBin)
		c.SendError(reqID, "server upgrade: self-check failed: "+err.Error())
		return
	}
	// 3) 自检通过 → 先回执成功 ACK(AE4):随后通过 setUpgradeMu 串行化换装。
	_ = c.SendResponse(reqID, map[string]interface{}{"ok": true, "verified": true})

	// 4) 换装:ACK 已在先,随后执行停旧 → .prev 备份 → 替换 → 重启(R6)。换装由接线
	//    方(SetUpgradeSwap)注入;upgradeMu 保证一次只有一个升级到达换装(防并发双 Exec)。
	if swap := c.server.upgradeSwap; swap != nil {
		c.server.upgradeMu.Lock()
		swap(tmpBin)
		c.server.upgradeMu.Unlock()
	} else {
		os.Remove(tmpBin)
	}
}

// verifyFileDigest 计算文件 sha256 并与期望值比对;期望为空视为缺失摘要而拒绝。
func verifyFileDigest(path, expected string) error {
	if expected == "" {
		return fmt.Errorf("missing expected digest")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return fmt.Errorf("sha256 %s != expected %s", got, expected)
	}
	return nil
}

// selfCheckBinary 用待装二进制自带的 version 子命令做「可启动」探测:子进程在超时内
// 以退出码 0 且产出非空解析输出方判定通过。语义上限为防损坏/防不可启动;真实性由 R2 的
// token 承担,此处不校验来源。
func selfCheckBinary(ctx context.Context, binPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("spawn binary: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("binary produced no parseable version output")
	}
	return nil
}

// handleRegisterExecutor 处理执行方注册/注销。
func (c *Client) handleRegisterExecutor(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	action := toString(payload["action"])
	version := toString(payload["version"])

	if _, ok := c.server.GetWatchDir(watchID); !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	switch action {
	case "add", "":
		c.server.RegisterExecutor(watchID, c.id, version)
	case "remove":
		c.server.UnregisterExecutor(watchID, c.id)
	default:
		c.SendError(msg.ID, "invalid action")
		return
	}
	c.SendResponse(msg.ID, map[string]interface{}{"ok": true, "watch_id": watchID, "executor": c.id})
}

// handleVersion 回答版本查询:返回中转自身的构建信息 + 各 watch 在线执行方的构建版本。
func (c *Client) handleVersion(msg protocol.Message) {
	nodes := []protocol.VersionInfo{{
		Role:      "transit",
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		Go:        runtime.Version(),
	}}

	for watchID, ver := range c.server.ExecutorVersions() {
		nodes = append(nodes, protocol.VersionInfo{
			Role:    "executor",
			WatchID: watchID,
			Version: ver,
		})
	}
	// executor 按 watch 排序,便于对比。
	sort.SliceStable(nodes[1:], func(i, j int) bool { return nodes[i+1].WatchID < nodes[j+1].WatchID })

	c.SendResponse(msg.ID, protocol.VersionResponse{OK: true, Nodes: nodes})
}

// handleStatus 处理 `relay status` 的连通性体检:对指定 watch 的执行方发探针并带回包超时,
// 测「中转→执行方」段时延,连同版本台账(futures)组装成 StatusResponse 回给请求方。
// 执行方不在线 / 探针超时时该段标「不可用」,整条命令仍成功——语义是「中转在线、远端掉线」。
func (c *Client) handleStatus(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])

	if _, ok := c.server.GetWatchDir(watchID); !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	nodes := []protocol.VersionInfo{{
		Role:      "transit",
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		Go:        runtime.Version(),
	}}
	for w, ver := range c.server.ExecutorVersions() {
		nodes = append(nodes, protocol.VersionInfo{
			Role:    "executor",
			WatchID: w,
			Version: ver,
		})
	}
	// executor 按 watch 排序,便于对比。
	sort.SliceStable(nodes[1:], func(i, j int) bool { return nodes[i+1].WatchID < nodes[j+1].WatchID })

	resp := protocol.StatusResponse{OK: true, Nodes: nodes}

	executorID, ok := c.server.GetExecutor(watchID)
	if !ok {
		resp.Seg2 = protocol.StatusSegment{Unavailable: "executor offline"}
		c.SendResponse(msg.ID, resp)
		return
	}

	probeID := uuid.New().String()
	probeCh := c.server.registerPending(probeID)
	// 无论回包成功、探针超时还是发送失败,都严格清理 pending 条目,不残留到连接关闭。
	defer c.server.deletePending(probeID)

	// 探针复用 MsgPing→MsgPong:执行方在自身连接上回 MsgPong(RequestID=probeID),由
	// server/client.go 的 case MsgPong 命中 pending 并投递。RTT 即「中转→执行方」段。
	start := time.Now()
	if err := c.server.SendTo(executorID, protocol.Message{Type: protocol.MsgPing, ID: probeID}); err != nil {
		resp.Seg2 = protocol.StatusSegment{Unavailable: "executor unavailable"}
		c.SendResponse(msg.ID, resp)
		return
	}

	select {
	case <-probeCh:
		latency := time.Since(start).Milliseconds()
		resp.Seg2 = protocol.StatusSegment{LatencyMS: &latency}
	case <-time.After(statusProbeTimeout):
		resp.Seg2 = protocol.StatusSegment{Unavailable: "executor probe timeout"}
	}

	c.SendResponse(msg.ID, resp)
}

// maybeResolvePending 把执行方的探针回包(带 RequestID 的 MsgPong)交给 `pending` 中的等待方。
// 命中即返回 true(已消费,不再向下处理)。
func (c *Client) maybeResolvePending(msg protocol.Message) bool {
	if msg.RequestID == "" {
		return false
	}
	ch, ok := c.server.resolvePending(msg.RequestID)
	if !ok {
		return false
	}
	select {
	case ch <- msg:
	default:
	}
	return true
}

// handlePushJob 处理流式 push-job 请求:有在线执行方则把元数据头转发给执行方,
// 并登记其 streamID 供后续内容帧透传;无执行方时回退为「中根本地暂存流式接收」。
func (c *Client) handlePushJob(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	relPath := toString(payload["rel_path"])
	streamID := toString(payload["stream_id"])

	if _, ok := c.server.GetWatchDir(watchID); !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}
	if streamID == "" {
		c.SendError(msg.ID, "push job: missing stream_id")
		return
	}

	if executorID, ok := c.server.GetExecutor(watchID); ok {
		c.server.SetReqOwner(msg.ID, c.id)
		c.server.SetPushRelay(streamID, pushRelayInfo{executorID: executorID, reqID: msg.ID, requesterID: c.id})
		if err := c.server.SendTo(executorID, msg); err != nil {
			c.server.ClearReqOwner(msg.ID)
			c.server.DeletePushRelay(streamID)
			c.SendError(msg.ID, "executor unavailable: "+err.Error())
		}
		return
	}

	// 回退:把本流式 push 视为中转自身的接收流,写入暂存目录,收尾时自动回执。
	dir, _ := c.server.GetWatchDir(watchID)
	fullPath := safePath(dir, relPath)
	if fullPath == "" {
		c.SendError(msg.ID, "path traversal detected")
		return
	}
	tmpPath := fullPath + ".tmp-" + streamID
	reqID := msg.ID
	c.streamMu.Lock()
	c.streams[streamID] = &ReceiveStream{
		streamID: streamID,
		path:     fullPath,
		tmpPath:  tmpPath,
		onDone: func() {
			// 内容帧收完后回执 prompt + 最终响应,请求方以 exec 语义收尾。
			_ = c.Send(protocol.Message{
				Type:      protocol.MsgExecOutput,
				RequestID: reqID,
				Payload:   protocol.ExecChunk{Seq: 1, Stdout: true, Data: "[staged to transit; no online executor, jobs not run]\n"},
			})
			_ = c.SendResponse(reqID, protocol.ExecResponse{ExitCode: 0})
		},
	}
	c.streamMu.Unlock()
}

// maybeRelayPushStream 把被转发给执行方的流式 push 内容帧原样透传(含二进制块),不本地处理。
func (c *Client) maybeRelayPushStream(msg *protocol.Message) bool {
	if msg.StreamID == "" {
		return false
	}
	info, ok := c.server.GetPushRelay(msg.StreamID)
	if !ok {
		return false
	}

	switch msg.Type {
	case protocol.MsgStreamData:
		var raw []byte
		var offset, chunk float64
		if payload, ok := msg.Payload.(map[string]interface{}); ok {
			if r, ok := payload["data"].([]byte); ok {
				raw = r
			}
			offset = toFloat64(payload["offset"])
			chunk = toFloat64(payload["chunk"])
		}
		// 干净 JSON(不含 data,避免 base64 膨胀)+ 原始压缩二进制帧。
		clean := protocol.Message{
			Type:     protocol.MsgStreamData,
			ID:       uuid.New().String(),
			StreamID: msg.StreamID,
			Payload: protocol.StreamData{
				StreamID: msg.StreamID,
				Offset:   int64(offset),
				Chunk:    int(chunk),
			},
		}
		_ = c.server.SendToBinary(info.executorID, clean, raw)
		return true
	case protocol.MsgStreamEnd:
		c.server.DeletePushRelay(msg.StreamID)
		_ = c.server.SendTo(info.executorID, *msg)
		return true
	}
	return false
}

// maybeRelayExecReply 处理消息是否是被转发 exec 的执行方回包,则转发回请求方。
// 判定依据:消息携带 reqOwner 中存在的 RequestID。返回 true 表示已拦截处理。
func (c *Client) maybeRelayExecReply(msg protocol.Message) bool {
	if msg.RequestID == "" {
		return false
	}

	owner, ok := c.server.GetReqOwner(msg.RequestID)
	if !ok {
		return false
	}

	switch msg.Type {
	case protocol.MsgExecOutput:
		_ = c.server.SendTo(owner, msg)
		return true
	case protocol.MsgResponse, protocol.MsgError:
		c.server.ClearReqOwner(msg.RequestID)
		_ = c.server.SendTo(owner, msg)
		return true
	default:
		return false
	}
}

// cleanupStreams 断连时清理该客户端未完成流的临时文件(如自升级暂存 .incoming),
// 避免客户端中途掉线(未发 MsgStreamEnd)留下孤儿临时文件。
func (c *Client) cleanupStreams() {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	for id, stream := range c.streams {
		if stream == nil {
			continue
		}
		if stream.tmpPath != "" {
			os.Remove(stream.tmpPath)
		}
		if stream.path != "" && stream.path != stream.tmpPath {
			os.Remove(stream.path)
		}
		delete(c.streams, id)
	}
}

// handleStreamData 处理流数据
func (c *Client) handleStreamData(msg protocol.Message) {
	c.streamMu.RLock()
	stream, ok := c.streams[msg.StreamID]
	c.streamMu.RUnlock()

	if !ok || stream == nil {
		return
	}

	payload, _ := msg.Payload.(map[string]interface{})
	offset := toFloat64(payload["offset"])

	if int64(offset) != stream.received {
		return
	}

	var chunkData []byte
	if raw, ok := payload["data"].([]byte); ok {
		decompressed, err := protocol.Decompress(raw)
		if err == nil {
			chunkData = decompressed
		} else {
			chunkData = raw
		}
	}

	stream.buf = append(stream.buf, chunkData...)
	stream.received += int64(len(chunkData))

	// 升级流上限:已超过声明大小即中止并清理,防身份合法客户端写满 /tmp。
	if stream.maxSize > 0 && stream.received > stream.maxSize {
		c.abortStream(msg.StreamID, stream, "received exceeds declared size")
		return
	}

	if len(stream.buf) > 64*1024 {
		os.MkdirAll(filepath.Dir(stream.tmpPath), 0755)
		f, err := os.OpenFile(stream.tmpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.Write(stream.buf)
			f.Close()
		}
		stream.buf = stream.buf[:0]
	}
}

// abortStream 中止一条在途接收流:清理临时文件并移除登记,防止超量/损坏流留下孤儿 /tmp 文件。
func (c *Client) abortStream(streamID string, stream *ReceiveStream, reason string) {
	if stream != nil {
		if stream.tmpPath != "" {
			os.Remove(stream.tmpPath)
		}
		if stream.path != "" && stream.path != stream.tmpPath {
			os.Remove(stream.path)
		}
	}
	c.streamMu.Lock()
	delete(c.streams, streamID)
	c.streamMu.Unlock()
}

// handleStreamEnd 处理流结束
func (c *Client) handleStreamEnd(msg protocol.Message) {
	c.streamMu.Lock()
	stream, ok := c.streams[msg.StreamID]
	delete(c.streams, msg.StreamID)
	c.streamMu.Unlock()

	if !ok || stream == nil {
		return
	}

	if len(stream.buf) > 0 {
		os.MkdirAll(filepath.Dir(stream.tmpPath), 0755)
		f, err := os.OpenFile(stream.tmpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			c.Send(protocol.Message{Type: protocol.MsgStreamEnd, StreamID: msg.StreamID, Payload: protocol.StreamEnd{StreamID: msg.StreamID, OK: false, Error: err.Error()}})
			return
		}
		f.Write(stream.buf)
		f.Close()
	}

	if err := os.Rename(stream.tmpPath, stream.path); err != nil {
		c.Send(protocol.Message{Type: protocol.MsgStreamEnd, StreamID: msg.StreamID, Payload: protocol.StreamEnd{StreamID: msg.StreamID, OK: false, Error: err.Error()}})
		return
	}

	// push-job 回退暂存收尾:落盘后回执 prompt + 最终响应。
	if stream.onDone != nil {
		stream.onDone()
	} else {
		c.Send(protocol.Message{Type: protocol.MsgStreamEnd, StreamID: msg.StreamID, Payload: protocol.StreamEnd{StreamID: msg.StreamID, OK: true, Received: stream.received}})
	}
}

// Send 发送消息
func (c *Client) Send(msg protocol.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.conn.WriteJSON(msg)
}

// SendBinary sends a JSON message followed by a binary frame
func (c *Client) SendBinary(msg protocol.Message, raw []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.conn.WriteJSON(msg); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, raw)
}

// SendResponse 发送响应
func (c *Client) SendResponse(requestID string, payload interface{}) error {
	return c.Send(protocol.Message{Type: protocol.MsgResponse, ID: uuid.New().String(), RequestID: requestID, Payload: payload})
}

// SendError 发送错误
func (c *Client) SendError(requestID, errMsg string) error {
	return c.Send(protocol.Message{Type: protocol.MsgError, ID: uuid.New().String(), RequestID: requestID, Payload: errMsg})
}

// ReceiveStream 接收流
type ReceiveStream struct {
	streamID string
	path     string
	tmpPath  string
	received int64
	buf      []byte
	// 升级流上限(0 = 不限制)。内容总量超过即中止,防 /tmp 被无限写满(post 仍受摘要门限)。
	maxSize int64
	// onDone 在流完整落盘(rename 成功)后触发;push-job 回退暂存用来自动回执。
	onDone func()
}

// handlePull 处理文件下载请求
func (c *Client) handlePull(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	path := toString(payload["path"])

	dir, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	fullPath := safePath(dir, path)
	if fullPath == "" {
		c.SendError(msg.ID, "path traversal detected")
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		c.SendError(msg.ID, err.Error())
		return
	}

	streamID := msg.StreamID
	if streamID == "" {
		streamID = uuid.New().String()
	}

	c.Send(protocol.Message{
		Type: protocol.MsgStreamStart, ID: uuid.New().String(), RequestID: msg.ID, StreamID: streamID,
		Payload: protocol.StreamStart{StreamID: streamID, Total: info.Size(), Offset: 0, Remaining: info.Size(), Compressed: true},
	})

	f, err := os.Open(fullPath)
	if err != nil {
		c.Send(protocol.Message{Type: protocol.MsgStreamEnd, ID: uuid.New().String(), StreamID: streamID, Payload: protocol.StreamEnd{StreamID: streamID, OK: false, Error: err.Error()}})
		return
	}
	defer f.Close()

	hasher := sha256.New()
	buf := make([]byte, protocol.DefaultChunkSize)
	var offset int64
	chunk := 0
	for {
		n, err := f.Read(buf)
		if n > 0 {
			hasher.Write(buf[:n])
			compressed := protocol.Compress(buf[:n])
			c.SendBinary(protocol.Message{
				Type: protocol.MsgStreamData, ID: uuid.New().String(), StreamID: streamID,
				Payload: protocol.StreamData{StreamID: streamID, Offset: offset, Chunk: chunk},
			}, compressed)
			offset += int64(n)
			chunk++
		}
		if err != nil {
			break
		}
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	c.Send(protocol.Message{
		Type: protocol.MsgStreamEnd, ID: uuid.New().String(), StreamID: streamID,
		Payload: protocol.StreamEnd{StreamID: streamID, OK: true, Received: offset, Digest: digest},
	})
}

// handlePush 处理文件上传请求
func (c *Client) handlePush(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	path := toString(payload["path"])

	dir, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	fullPath := safePath(dir, path)
	if fullPath == "" {
		c.SendError(msg.ID, "path traversal detected")
		return
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		c.SendError(msg.ID, fmt.Sprintf("mkdir: %v", err))
		return
	}

	streamID := msg.StreamID
	if streamID == "" {
		streamID = toString(payload["stream_id"])
	}

	tmpPath := fullPath + ".tmp-" + streamID
	c.streamMu.Lock()
	c.streams[streamID] = &ReceiveStream{
		streamID: streamID,
		path:     fullPath,
		tmpPath:  tmpPath,
	}
	c.streamMu.Unlock()

	c.SendResponse(msg.ID, map[string]interface{}{"ok": true, "stream_id": streamID})
}

// handleSubscribe 处理订阅请求
func (c *Client) handleSubscribe(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	action := toString(payload["action"])

	_, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendResponse(msg.ID, protocol.SubscribedResponse{WatchID: watchID, OK: false, Error: "unknown watch_id"})
		return
	}

	switch action {
	case "add", "":
		c.server.Subscribe(c.id, watchID)
	case "remove":
		c.server.Unsubscribe(c.id, watchID)
	default:
		c.SendResponse(msg.ID, protocol.SubscribedResponse{WatchID: watchID, OK: false, Error: "invalid action"})
		return
	}

	c.SendResponse(msg.ID, protocol.SubscribedResponse{WatchID: watchID, OK: true})
}

// safePath resolves and validates that the joined path is within baseDir.
// Returns empty string if path traversal is detected.
func safePath(baseDir, relPath string) string {
	absBase := filepath.Clean(baseDir)
	full := filepath.Clean(filepath.Join(absBase, relPath))
	if !strings.HasPrefix(full, absBase+string(filepath.Separator)) && full != absBase {
		return ""
	}
	return full
}

func readDir(path string) ([]protocol.FileEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := make([]protocol.FileEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		if info == nil {
			continue
		}
		result = append(result, protocol.FileEntry{
			Name: e.Name(), Path: filepath.Join(path, e.Name()),
			IsDir: e.IsDir(), Size: info.Size(),
			ModTime: info.ModTime().UnixMilli(),
			Mode:    uint32(info.Mode()),
		})
	}
	return result, nil
}

func toFloat64(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]interface{}); ok {
		result := make([]byte, len(b))
		for i, e := range b {
			result[i] = byte(toFloat64(e))
		}
		return result
	}
	return nil
}
