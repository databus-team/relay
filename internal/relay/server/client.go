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
	"strings"
	"sync"
	"time"

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
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		
		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			c.SendError("", "invalid message")
			continue
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

// handleExec 处理执行请求
func (c *Client) handleExec(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	watchID := toString(payload["watch_id"])
	cmdStr := toString(payload["cmd"])
	timeout := int(toFloat64(payload["timeout"]))
	
	if timeout <= 0 { timeout = 30 }
	
	dir, ok := c.server.GetWatchDir(watchID)
	if !ok {
		c.SendError(msg.ID, "unknown watch_id")
		return
	}
	
	if strings.TrimSpace(cmdStr) == "" {
		c.SendError(msg.ID, "empty command")
		return
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	
	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = dir
	cmd.Stdin = nil
	
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		c.SendResponse(msg.ID, protocol.ExecResponse{
			ExitCode: 1,
			Stderr:   err.Error(),
			Duration: time.Since(start).Milliseconds(),
		})
		return
	}
	
	stdoutBytes, _ := io.ReadAll(stdout)
	stderrBytes, _ := io.ReadAll(stderr)
	cmd.Wait()
	
	exitCode := 0
	if ctx.Err() != nil {
		exitCode = -1
	} else {
		exitCode = cmd.ProcessState.ExitCode()
	}
	
	c.SendResponse(msg.ID, protocol.ExecResponse{
		ExitCode: exitCode,
		Stdout:   string(stdoutBytes),
		Stderr:   string(stderrBytes),
		Duration: time.Since(start).Milliseconds(),
	})
}

// handleStreamData 处理流数据
func (c *Client) handleStreamData(msg protocol.Message) {
	c.streamMu.RLock()
	stream, ok := c.streams[msg.StreamID]
	c.streamMu.RUnlock()
	
	if !ok || stream == nil { return }
	
	payloadData, err := json.Marshal(msg.Payload)
	if err != nil { return }
	var sd protocol.StreamData
	if err := json.Unmarshal(payloadData, &sd); err != nil { return }
	
	if sd.Offset != stream.received { return }
	
	chunkData, err := protocol.Decompress(sd.Data)
	if err != nil {
		chunkData = sd.Data
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
	
	if !ok || stream == nil { return }
	
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
	
	c.Send(protocol.Message{Type: protocol.MsgStreamEnd, StreamID: msg.StreamID, Payload: protocol.StreamEnd{StreamID: msg.StreamID, OK: true, Received: stream.received}})
}

// Send 发送消息
func (c *Client) Send(msg protocol.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.conn.WriteJSON(msg)
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
			c.Send(protocol.Message{
				Type: protocol.MsgStreamData, ID: uuid.New().String(), StreamID: streamID,
				Payload: protocol.StreamData{StreamID: streamID, Offset: offset, Data: compressed, Chunk: chunk},
			})
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
	if err != nil { return nil, err }
	
	result := make([]protocol.FileEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		if info == nil { continue }
		result = append(result, protocol.FileEntry{
			Name: e.Name(), Path: filepath.Join(path, e.Name()),
			IsDir: e.IsDir(), Size: info.Size(),
			ModTime: info.ModTime().UnixMilli(),
			Mode: uint32(info.Mode()),
		})
	}
	return result, nil
}


func toFloat64(v interface{}) float64 {
	if f, ok := v.(float64); ok { return f }
	return 0
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]interface{}); ok {
		result := make([]byte, len(b))
		for i, e := range b { result[i] = byte(toFloat64(e)) }
		return result
	}
	return nil
}
