package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

type Client struct {
	url       string
	token     string
	watchID   string
	conn      *websocket.Conn
	connected atomic.Bool

	sendCh      chan sendMsg
	recvCh      chan *protocol.Message
	eventCh     chan protocol.FileEvent
	pending     map[string]chan *protocol.Response
	pendingMu   sync.RWMutex

	closeCh      chan struct{}
	reconnectCfg ReconnectConfig
	lastPong     atomic.Int64

	streamMu   sync.RWMutex
	streams    map[string]*receiveStream
	streamDone map[string]chan error
}

func New(url, token, watchID string) (*Client, error) {
	return &Client{
		url:          url,
		token:        token,
		watchID:      watchID,
		sendCh:       make(chan sendMsg, 100),
		recvCh:       make(chan *protocol.Message, 100),
		eventCh:      make(chan protocol.FileEvent, 100),
		pending:      make(map[string]chan *protocol.Response),
		closeCh:      make(chan struct{}),
		reconnectCfg: DefaultReconnectConfig(),
		streams:      make(map[string]*receiveStream),
		streamDone:   make(map[string]chan error),
	}, nil
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
	conn, _, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.conn = conn

	connectReq := &protocol.Message{
		Type: protocol.MsgConnect,
		ID:   uuid.New().String(),
		Payload: protocol.ConnectRequest{
			ClientID:  uuid.New().String(),
			Token:     c.token,
			Version:   1,
			Subscribe: []string{c.watchID},
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

	return nil
}

func (c *Client) Disconnect() error {
	if c.conn != nil {
		c.conn.Close()
	}
	c.connected.Store(false)
	return nil
}

func (c *Client) CloseCh() <-chan struct{} {
	return c.closeCh
}

func (c *Client) readLoop() {
	for {
		msgType, data, err := c.conn.ReadMessage()
		if err != nil {
			c.connected.Store(false)
			go c.reconnectLoop(context.Background())
			return
		}

		if msgType == websocket.BinaryMessage {
			continue
		}

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Type == protocol.MsgStreamData {
			binType, binData, err := c.conn.ReadMessage()
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
	for sm := range c.sendCh {
		if err := c.conn.WriteJSON(sm.Message); err != nil {
			continue
		}
		if len(sm.Raw) > 0 {
			if err := c.conn.WriteMessage(websocket.BinaryMessage, sm.Raw); err != nil {
				continue
			}
		}
	}
}

func (c *Client) handleMessage(msg protocol.Message) {
	switch msg.Type {
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

func (c *Client) Exec(ctx context.Context, cmd string, cwd string, timeout int) (*protocol.ExecResponse, error) {
	if timeout <= 0 {
		timeout = 30
	}

	resp, err := c.Request(ctx, protocol.MsgExec, protocol.ExecRequest{
		WatchID: c.watchID,
		Cmd:     cmd,
		Cwd:     cwd,
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}

	data, _ := json.Marshal(resp.Payload)
	var execResp protocol.ExecResponse
	if err := json.Unmarshal(data, &execResp); err != nil {
		return nil, fmt.Errorf("parse exec response: %w", err)
	}

	return &execResp, nil
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
		WatchID: c.watchID,
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
		WatchID: c.watchID,
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
	if s, ok := v.(string); ok { return s }
	return ""
}
func toBool(v interface{}) bool {
	if b, ok := v.(bool); ok { return b }
	return false
}
func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case float64: return int64(val)
	case int64: return val
	}
	return 0
}
