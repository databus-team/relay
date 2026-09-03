package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

type Client struct {
	url       string
	token     string
	id        string
	headers   http.Header // WebSocket 握手携带的自定义头(中转前置鉴权)
	conn      *websocket.Conn
	connMu    sync.RWMutex // 保护 conn:持久的写循环在重连后会写新 conn
	connected atomic.Bool

	sendCh    chan sendMsg
	recvCh    chan *protocol.Message
	eventCh   chan protocol.FileEvent
	pending   map[string]chan *protocol.Response
	pendingMu sync.RWMutex

	closeCh      chan struct{}
	reconnectCfg ReconnectConfig
	lastPong     atomic.Int64
	heartbeatGen atomic.Int64 // 每次重连成功后递增,使旧心跳 goroutine 失效(superseded)后自行退出

	streamMu   sync.RWMutex
	streams    map[string]*receiveStream
	streamDone map[string]chan error

	execMu          sync.RWMutex
	execStreams     map[string]chan *protocol.Message // reqID -> 请求方流式 exec 通道
	execHandler     func(*ExecSession)                // 执行方入站 exec 处理回调
	pushJobHandler  func(*PushJobSession)             // 执行方入站 push-job 处理回调
	pushJobHandlerM sync.RWMutex
	execHandlerM    sync.RWMutex

	configSyncHandler func(*ConfigSyncSession) // 执行方入站 config-sync 处理回调
	configSyncM       sync.RWMutex

	pushRecvMu sync.RWMutex
	pushRecv   map[string]*inboundPushReceive // streamID -> 入站流式 push 接收状态

	tunnelHandler  func(*TunnelSession) // 执行方入站隧道建连回调
	tunnelHandlerM sync.RWMutex
	tunnelConns    map[string]*tunnelConn   // 入站隧道(executor):streamID -> 真实 TCP
	tunnelStreams  map[string]*TunnelStream // 出站隧道(requester):streamID -> 本地 SOCKS/SOCKS 通
	tunnelMu       sync.RWMutex

	onReconnect   func() // 重连成功后的回调(执行方用于重新注册)
	onReconnectMu sync.Mutex
}

// Option 配置新建 Client 的行为(如 WebSocket 握手自定义头)。
type Option func(*Client)

// WithHeaders 设置 WebSocket 握手携带的 HTTP 头(应对中转前置的 headers 鉴权)。
func WithHeaders(h http.Header) Option {
	return func(c *Client) { c.headers = h }
}

func New(url, token, id string, opts ...Option) (*Client, error) {
	c := &Client{
		url:           url,
		token:         token,
		id:            id,
		sendCh:        make(chan sendMsg, 100),
		recvCh:        make(chan *protocol.Message, 100),
		eventCh:       make(chan protocol.FileEvent, 100),
		pending:       make(map[string]chan *protocol.Response),
		closeCh:       make(chan struct{}),
		reconnectCfg:  DefaultReconnectConfig(),
		streams:       make(map[string]*receiveStream),
		streamDone:    make(map[string]chan error),
		execStreams:   make(map[string]chan *protocol.Message),
		pushRecv:      make(map[string]*inboundPushReceive),
		tunnelConns:   make(map[string]*tunnelConn),
		tunnelStreams: make(map[string]*TunnelStream),
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *Client) Connect(ctx context.Context) error {
	if err := c.dial(ctx); err != nil {
		return err
	}
	c.connected.Store(true)
	go c.readLoop()
	go c.writeLoop()
	go c.startHeartbeat(ctx)
	return nil
}

func (c *Client) dial(ctx context.Context) error {
	dialer := websocket.Dialer{}
	conn, httpResp, err := dialer.DialContext(ctx, c.url, c.headers)
	if err != nil {
		if httpResp != nil {
			return fmt.Errorf("dial: %w (upstream HTTP %s)", err, httpResp.Status)
		}
		return fmt.Errorf("dial: %w", err)
	}

	connectReq := &protocol.Message{
		Type: protocol.MsgConnect,
		ID:   uuid.New().String(),
		Payload: protocol.ConnectRequest{
			ClientID:  uuid.New().String(),
			Token:     c.token,
			Version:   1,
			Subscribe: []string{c.id},
		},
	}

	if err := conn.WriteJSON(connectReq); err != nil {
		conn.Close()
		return err
	}

	var resp protocol.Message
	if err := conn.ReadJSON(&resp); err != nil {
		conn.Close()
		return err
	}

	if resp.Type != protocol.MsgConnectAck {
		conn.Close()
		return fmt.Errorf("unexpected message type: %s", resp.Type)
	}

	c.setConn(conn)
	return nil
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *Client) getConn() *websocket.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

func (c *Client) Disconnect() error {
	if conn := c.getConn(); conn != nil {
		conn.Close()
	}
	c.connected.Store(false)
	return nil
}

func (c *Client) CloseCh() <-chan struct{} {
	return c.closeCh
}

// readDeadline 是单次 WS 读的续读间隔(由每一条入站帧刷新,含心跳 pong)。中转经 NAT/代理
// 重启时,执行器这边往往拿不到 EOF/RST,ReadMessage 会一直阻塞——靠它把半开连接强制判定为
// 读超时 → readLoop 走重连,不必再死等心跳的 pong 计龄(见 heartbeatTimeout)兜底。
const readDeadline = 45 * time.Second

func (c *Client) readLoop() {
	// 每个连接各有一个 readLoop,只读它自己被创建时绑定的 conn(重连后换新 conn/新 readLoop)。
	conn := c.getConn()
	_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			c.connected.Store(false)
			// 传输断开:先关重隧道(拆解流中止/入站连接关闭),避免重连后本地仍以为隧道黑盒仍开着。
			c.closeAllTunnels("transport lost")
			go c.reconnectLoop(context.Background())
			return
		}

		// 收到任一帧(含心跳 pong)说明连接还活着,续期读 deadline,把超时窗口前移。
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))

		if msgType == websocket.BinaryMessage {
			continue
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Type == protocol.MsgStreamData {
			binType, binData, err := conn.ReadMessage()
			if err != nil {
				continue
			}
			if binType == websocket.BinaryMessage {
				c.attachBinaryToStreamData(&msg, binData)
			}
		}

		c.handleMessage(msg)
	}
}

func (c *Client) attachBinaryToStreamData(msg *protocol.Message, raw []byte) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	payload["data"] = raw
}

func (c *Client) writeLoop() {
	// writeLoop 是单例(仅 Connect 启动一次),随 sendCh 常驻;每次写都取当前 conn,
	// 重连后自动写新连接。绝不重复启动,否则两个 goroutine 并发写同一 conn 会 panic。
	for sm := range c.sendCh {
		conn := c.getConn()
		if conn == nil {
			continue
		}
		if err := conn.WriteJSON(sm.Message); err != nil {
			continue
		}
		if len(sm.Raw) > 0 {
			if err := conn.WriteMessage(websocket.BinaryMessage, sm.Raw); err != nil {
				continue
			}
		}
	}
}

// SetOnReconnect 设置重连成功后的回调(执行方用于向中转重新注册)。
func (c *Client) SetOnReconnect(fn func()) {
	c.onReconnectMu.Lock()
	c.onReconnect = fn
	c.onReconnectMu.Unlock()
}

// fireOnReconnect 触发重连回调。由 reconnectLoop 在 re-dial 成功后调用。
func (c *Client) fireOnReconnect() {
	c.onReconnectMu.Lock()
	fn := c.onReconnect
	c.onReconnectMu.Unlock()
	if fn != nil {
		go fn()
	}
}

// sendMessage 将一条消息入队（供入站 exec 会话回包使用）。
func (c *Client) sendMessage(msg *protocol.Message) error {
	c.sendCh <- sendMsg{Message: msg}
	return nil
}

func (c *Client) handleMessage(msg protocol.Message) {
	// 请求方流式 exec:命中 execStreams 的输出/收尾帧直接投递,不落入 pending
	if c.routeExecStream(&msg) {
		return
	}

	// 执行方入站流式 push:命中 pushRecv 的内容/结束帧直接写盘消费。
	if c.routeInboundPushStream(&msg) {
		return
	}

	switch msg.Type {
	case protocol.MsgExec:
		c.handleInboundExec(msg)
	case protocol.MsgTunnelConnect:
		c.handleInboundTunnelConnect(msg)
	case protocol.MsgTunnelData:
		c.handleInboundTunnelData(msg)
	case protocol.MsgTunnelEnd:
		c.handleInboundTunnelEnd(msg)
	case protocol.MsgPushJob:
		c.handleInboundPushJob(msg)
	case protocol.MsgConfigSync:
		c.handleInboundConfigSync(msg)
	case protocol.MsgResponse, protocol.MsgError:
		if msg.RequestID != "" {
			c.pendingMu.RLock()
			ch, ok := c.pending[msg.RequestID]
			c.pendingMu.RUnlock()
			if ok {
				var resp protocol.Response
				resp.RequestID = msg.RequestID
				if msg.Type == protocol.MsgError {
					resp.OK = false
					if s, ok := msg.Payload.(string); ok {
						resp.Error = s
					}
				} else {
					resp.OK = true
					resp.Payload = msg.Payload
				}
				ch <- &resp
			}
		}
	case protocol.MsgFileEvent:
		if payload, ok := msg.Payload.(map[string]interface{}); ok {
			data, _ := json.Marshal(payload)
			var event protocol.FileEvent
			json.Unmarshal(data, &event)
			select {
			case c.eventCh <- event:
			default:
			}
		}
	case protocol.MsgPing:
		// 作为执行方应答中转探针:回响 MsgPong(RequestID 对应入站 MsgPing 的 ID)。
		// 经发送队列出站(比直写连接安全),镜像 server/client.go 的 MsgPing→MsgPong 写法。
		// 用非阻塞投递:发送队列满时丢弃。探针本就允许超时降级(中转把该段标不可用),
		// 不因探针回包阻塞执行方自身的读写循环(避免 status 风暴挤掉正常心跳/exec 帧)。
		select {
		case c.sendCh <- sendMsg{Message: &protocol.Message{
			Type:      protocol.MsgPong,
			ID:        uuid.New().String(),
			RequestID: msg.ID,
		}}:
		default:
		}
	case protocol.MsgPong:
		c.lastPong.Store(timeNow())
		if msg.RequestID != "" {
			c.pendingMu.RLock()
			ch, ok := c.pending[msg.RequestID]
			c.pendingMu.RUnlock()
			if ok {
				ch <- &protocol.Response{RequestID: msg.RequestID, OK: true}
			}
		}
	case protocol.MsgStreamStart:
		c.handleStreamStart(toClientMsg(msg))
	case protocol.MsgStreamData:
		c.handleStreamData(toClientMsg(msg))
	case protocol.MsgStreamEnd:
		// Check if this is a response to an outbound push stream
		if msg.StreamID != "" {
			c.streamMu.RLock()
			doneCh, ok := c.streamDone[msg.StreamID]
			c.streamMu.RUnlock()
			if ok {
				payload, _ := msg.Payload.(map[string]interface{})
				var se protocol.StreamEnd
				data, _ := json.Marshal(payload)
				json.Unmarshal(data, &se)
				if se.OK {
					doneCh <- nil
				} else {
					doneCh <- fmt.Errorf("stream error: %s", se.Error)
				}
				c.streamMu.Lock()
				delete(c.streamDone, msg.StreamID)
				c.streamMu.Unlock()
				return
			}
		}
		// Otherwise it's for an inbound pull stream
		c.handleStreamEnd(toClientMsg(msg))
	}
}

func (c *Client) Request(ctx context.Context, msgType protocol.MessageType, payload interface{}) (*protocol.Response, error) {
	reqID := uuid.New().String()
	msg := &protocol.Message{Type: msgType, ID: reqID, Payload: payload}

	respCh := make(chan *protocol.Response, 1)
	c.pendingMu.Lock()
	c.pending[reqID] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	select {
	case c.sendCh <- sendMsg{Message: msg}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) sendAndWait(ctx context.Context, msg *protocol.Message) (*protocol.Response, error) {
	respCh := make(chan *protocol.Response, 1)
	c.pendingMu.Lock()
	c.pending[msg.ID] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, msg.ID)
		c.pendingMu.Unlock()
	}()

	select {
	case c.sendCh <- sendMsg{Message: msg}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) failAllPending(reason string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for id, ch := range c.pending {
		ch <- &protocol.Response{RequestID: id, OK: false, Error: reason}
		delete(c.pending, id)
	}
}

// RegisterExecutor 向服务端注册(或注销,action="remove")本客户端为某执行方。
// executorID 即执行方节点身份(backend.config.executor_id)。
func (c *Client) RegisterExecutor(ctx context.Context, executorID, action, buildVersion string) error {
	resp, err := c.Request(ctx, protocol.MsgRegisterExecutor, protocol.RegisterExecutorRequest{
		ExecutorID: executorID,
		Action:     action,
		Version:    buildVersion,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("register executor failed: %s", resp.Error)
	}
	return nil
}

// Version 向中转查询版本台账:中转自身构建信息 + 各 watch 在线执行方版本,用于跨机对比。
func (c *Client) Version(ctx context.Context) (protocol.VersionResponse, error) {
	var vr protocol.VersionResponse
	resp, err := c.Request(ctx, protocol.MsgVersion, nil)
	if err != nil {
		return vr, err
	}
	if !resp.OK {
		return vr, fmt.Errorf("version query failed: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Payload)
	if err := json.Unmarshal(data, &vr); err != nil {
		return vr, fmt.Errorf("decode version response: %w", err)
	}
	return vr, nil
}

// Status 向中转查询整座部署的连通状态:中转对每个执行方的探针结果(Executors)与中转节点
// 信息(Transit)。段1(本地→中转往返)由请求方本地 Ping 计时、各执行方的累计(Total)由
// 请求方按 段1+段2 求和,故此处只返回中转侧组装的结果。
func (c *Client) Status(ctx context.Context) (protocol.StatusResponse, error) {
	var st protocol.StatusResponse
	resp, err := c.Request(ctx, protocol.MsgStatus, protocol.StatusRequest{})
	if err != nil {
		return st, err
	}
	if !resp.OK {
		return st, fmt.Errorf("transit status failed: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Payload)
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("decode status response: %w", err)
	}
	return st, nil
}

func (c *Client) Subscribe(ctx context.Context, watchID string) error {
	resp, err := c.Request(ctx, protocol.MsgSubscribe, protocol.SubscribeRequest{
		WatchID: watchID,
		Action:  "add",
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("subscribe failed: %s", resp.Error)
	}
	return nil
}

func (c *Client) List(ctx context.Context, path string) ([]protocol.FileEntry, error) {
	resp, err := c.Request(ctx, protocol.MsgList, protocol.ListRequest{
		WatchID: c.id,
		Path:    path,
	})
	if err != nil {
		return nil, err
	}

	data, _ := json.Marshal(resp.Payload)
	var listResp protocol.ListResponse
	if err := json.Unmarshal(data, &listResp); err != nil {
		payload, ok := resp.Payload.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid list response")
		}
		entriesData, _ := payload["entries"].([]interface{})
		entries := make([]protocol.FileEntry, 0, len(entriesData))
		for _, e := range entriesData {
			em := e.(map[string]interface{})
			entries = append(entries, protocol.FileEntry{
				Name:    toString(em["name"]),
				Path:    toString(em["path"]),
				IsDir:   toBool(em["is_dir"]),
				Size:    toInt64(em["size"]),
				ModTime: toInt64(em["mod_time"]),
				Mode:    uint32(toInt64(em["mode"])),
			})
		}
		return entries, nil
	}

	return listResp.Entries, nil
}

func (c *Client) Delete(ctx context.Context, path string) error {
	_, err := c.Request(ctx, protocol.MsgDelete, protocol.DeleteRequest{
		WatchID: c.id,
		Path:    path,
	})
	return err
}

func (c *Client) EventCh() <-chan protocol.FileEvent {
	return c.eventCh
}

func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

func (c *Client) SetReconnectEnabled(enabled bool) {
	c.reconnectCfg.Enabled = enabled
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Request(ctx, protocol.MsgPing, nil)
	return err
}

func toClientMsg(msg protocol.Message) Message {
	return Message{
		Type:      msg.Type,
		ID:        msg.ID,
		RequestID: msg.RequestID,
		Payload:   msg.Payload,
		StreamID:  msg.StreamID,
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func toBool(v interface{}) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	}
	return 0
}
