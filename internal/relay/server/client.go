package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
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

	case protocol.MsgPushJob:
		c.handlePushJob(msg)

	case protocol.MsgSubscribe:
		c.handleSubscribe(msg)

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

// handleExec 处理执行请求 —— 纯透明转发给该 watch 的已注册执行方。
// 中转不本地执行;无执行方时直接报错。
func (c *Client) handleExec(msg protocol.Message) {
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

	// 记录请求方归属,供执行方回流帧转发回去;原样转发 msg(保留 msg.ID 作关联)
	c.server.SetReqOwner(msg.ID, c.id)
	if err := c.server.SendTo(executorID, msg); err != nil {
		c.server.ClearReqOwner(msg.ID)
		c.SendError(msg.ID, "executor unavailable: "+err.Error())
	}
}

// handleRegisterExecutor 处理执行方注册/注销。
func (c *Client) handleRegisterExecutor(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	action := toString(payload["action"])

	if _, ok := c.server.GetWatchDir(watchID); !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}

	switch action {
	case "add", "":
		c.server.RegisterExecutor(watchID, c.id)
	case "remove":
		c.server.UnregisterExecutor(watchID, c.id)
	default:
		c.SendError(msg.ID, "invalid action")
		return
	}
	c.SendResponse(msg.ID, map[string]interface{}{"ok": true, "watch_id": watchID, "executor": c.id})
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
