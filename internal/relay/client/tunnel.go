package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

// tunnelConnectTimeout 等待隧道建连确认(executor 白名单校验 + 真实连接)的上限。
// 超时即放弃并把该 stream 的注册表条目拆除,避免请求方无界挂起或泄漏中转槽位(与 status 探针的分析同构)。
const tunnelConnectTimeout = 15 * time.Second

// tunnelWriteTimeout 是 executor 向真实目标写数据时的单次写上限,防止慢/停读目标把处理方读循环无限阻塞。
const tunnelWriteTimeout = 60 * time.Second

// 隧道在客户端侧的两条通路:
//   - 入站(executor 角色):(c.handleInboundTunnelConnect)一端收建连,白名单 + 真实连接,经
//     tunnelConns(streamID → 真实 TCP 连接)承载双向字节泵。
//   - 出站(请求方/本地 tunnel CLI):TunnelOpen 打开隧道,经 tunnelStreams(streamID → 本地
//     SOCKS 连接通道)承载双向字节泵。
// 两表互斥(同一条隧道只会是一个端点),但按 streamID 独立、天然支持多隧道并行。

// tunnelConn 一条入站隧道落到 executor 的真实 TCP 连接;写用互斥锁串行化。
type tunnelConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (tc *tunnelConn) Write(p []byte) (int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	// 写加上限:慢/停读目标最多阻塞一个写周期,避免把处理方 readLoop 无限卡死(把无限阻塞转成有界错误)。
	_ = tc.conn.SetWriteDeadline(time.Now().Add(tunnelWriteTimeout))
	n, err := tc.conn.Write(p)
	_ = tc.conn.SetWriteDeadline(time.Time{})
	return n, err
}

// SetTunnelHandler 设置入站隧道建连回调(执行方角色)。设置了它,该客户端才会处理 MsgTunnelConnect。
func (c *Client) SetTunnelHandler(fn func(*TunnelSession)) {
	c.tunnelHandlerM.Lock()
	c.tunnelHandler = fn
	c.tunnelHandlerM.Unlock()
}

// TunnelSession 一次入站隧道建连(executor 视角):目标/出口 watch + 已建立通道后落连。
type TunnelSession struct {
	client    *Client
	requestID string
	streamID  string
	watchID   string
	target    string
	port      uint16
}

func (s *TunnelSession) StreamID() string { return s.streamID }
func (s *TunnelSession) WatchID() string  { return s.watchID }
func (s *TunnelSession) Target() string   { return s.target }
func (s *TunnelSession) Port() uint16     { return s.port }

// Accept 建连成功:登记真实连接、回执成功 ACK,并启动「连接 → MsgTunnelData」泵。
// 反向(MsgTunnelData → 连接)由 handleInboundTunnelData 处理。EOF/错误 → MsgTunnelEnd + 清理。
func (s *TunnelSession) Accept(conn net.Conn) {
	c := s.client
	c.tunnelMu.Lock()
	c.tunnelConns[s.streamID] = &tunnelConn{conn: conn}
	c.tunnelMu.Unlock()

	_ = c.sendMessage(&protocol.Message{
		Type:      protocol.MsgResponse,
		ID:        uuid.New().String(),
		RequestID: s.requestID,
		Payload:   protocol.Response{OK: true},
	})

	go s.pumpConnToRelay(conn)
}

// Reject 建连失败(白名单未命中/dial 失败等):回执失败,不建立内网连接。
func (s *TunnelSession) Reject(errMsg string) {
	_ = s.client.sendMessage(&protocol.Message{
		Type:      protocol.MsgError,
		ID:        uuid.New().String(),
		RequestID: s.requestID,
		Payload:   "tunnel: " + errMsg,
	})
}

// pumpConnToRelay 读真实连接,把字节作为 MsgTunnelData 发出;EOF/错误 → 发 MsgTunnelEnd 并退盘。
func (s *TunnelSession) pumpConnToRelay(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			piece := make([]byte, n)
			copy(piece, buf[:n])
			_ = s.client.sendMessage(&protocol.Message{
				Type:     protocol.MsgTunnelData,
				ID:       uuid.New().String(),
				StreamID: s.streamID,
				Payload:  protocol.TunnelData{StreamID: s.streamID, Data: piece},
			})
		}
		if err != nil {
			break
		}
	}
	_ = s.client.sendMessage(&protocol.Message{
		Type:     protocol.MsgTunnelEnd,
		ID:       uuid.New().String(),
		StreamID: s.streamID,
		Payload:  protocol.TunnelEnd{StreamID: s.streamID},
	})
	s.client.takeInboundTunnel(s.streamID)
}

// takeInboundTunnel 移除并关闭一条入站隧道连接。
func (c *Client) takeInboundTunnel(streamID string) {
	c.tunnelMu.Lock()
	tc := c.tunnelConns[streamID]
	delete(c.tunnelConns, streamID)
	c.tunnelMu.Unlock()
	if tc != nil {
		tc.conn.Close()
	}
}

// handleInboundTunnelConnect 处理从中转转发来的建连请求(executor 视角)。
func (c *Client) handleInboundTunnelConnect(msg protocol.Message) {
	raw, _ := json.Marshal(msg.Payload)
	var req protocol.TunnelConnectRequest
	_ = json.Unmarshal(raw, &req)

	c.tunnelHandlerM.RLock()
	h := c.tunnelHandler
	c.tunnelHandlerM.RUnlock()
	if h == nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "tunnel executor not enabled"})
		return
	}
	if req.StreamID == "" {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "tunnel: missing stream_id"})
		return
	}
	sess := &TunnelSession{
		client:    c,
		requestID: msg.ID,
		streamID:  req.StreamID,
		watchID:   req.WatchID,
		target:    req.Target,
		port:      req.Port,
	}
	go h(sess)
}

// handleInboundTunnelData 入站数据帧:命中入站隧道 → 写真实连接;否则投递给出站(requester)流。
func (c *Client) handleInboundTunnelData(msg protocol.Message) {
	streamID := msg.StreamID
	if streamID == "" {
		return
	}
	c.tunnelMu.RLock()
	tc := c.tunnelConns[streamID]
	c.tunnelMu.RUnlock()
	if tc != nil {
		var td protocol.TunnelData
		raw, _ := json.Marshal(msg.Payload)
		_ = json.Unmarshal(raw, &td)
		if len(td.Data) > 0 {
			if _, err := tc.Write(td.Data); err != nil {
				// 目标写失败(停读/超时/对端关闭):拆除该隧道并把关闭通知给远端,防止读循环持续阻塞。
				c.takeInboundTunnel(streamID)
				_ = c.sendMessage(&protocol.Message{
					Type:     protocol.MsgTunnelEnd,
					ID:       uuid.New().String(),
					StreamID: streamID,
					Payload:  protocol.TunnelEnd{StreamID: streamID, Reason: "target write failed: " + err.Error()},
				})
			}
		}
		return
	}
	if ts := c.getTunnelStream(streamID); ts != nil {
		var td protocol.TunnelData
		raw, _ := json.Marshal(msg.Payload)
		_ = json.Unmarshal(raw, &td)
		ts.deliver(td.Data)
	}
}

// handleInboundTunnelEnd 结束帧:关停入站连接;出站流则置结束信号。
func (c *Client) handleInboundTunnelEnd(msg protocol.Message) {
	streamID := msg.StreamID
	if streamID == "" {
		return
	}
	c.takeInboundTunnel(streamID)
	if ts := c.takeTunnelStream(streamID); ts != nil {
		ts.close()
	}
}

// ---- 出站(requester)侧 ----

// tunnelStream 供本地 relay tunnel 使用的一条出站隧道:远端 → 本地的字节走 ch;
// 本地 → 远端的字节经 Send 发出。
type TunnelStream struct {
	client  *Client
	ID      string
	dataCh  chan []byte
	closeCh chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

// TunnelOpen 打开一条离站的 SOCKS5 隧道(requester 视角):发 MsgTunnelConnect 并等待建连确认。
// 成功返回 *TunnelStream;失败(执行方不可达/白名单拒/无在线执行方/超时)返回错误,并把该 stream
// 的中转注册表条目拆除(发 MsgTunnelEnd),避免被拒/超时建连泄漏中转槽位(耗尽 maxTunnels)。
func (c *Client) TunnelOpen(ctx context.Context, watchID, target string, port uint16) (*TunnelStream, error) {
	id := uuid.New().String()
	msg := &protocol.Message{
		Type:     protocol.MsgTunnelConnect,
		ID:       id,
		StreamID: id,
		Payload: protocol.TunnelConnectRequest{
			WatchID:  watchID,
			Target:   target,
			Port:     port,
			StreamID: id,
		},
	}

	// 建连确认只在等待期内有效;超时视为失败并拆除该 stream(复用 sendAndWait 的 pending/send 语义,
	// 但就建连这一等待单独加上限,防止对黑洞目标无界挂起)。
	waitCtx, cancel := context.WithTimeout(ctx, tunnelConnectTimeout)
	defer cancel()
	resp, err := c.sendAndWait(waitCtx, msg)
	if err != nil {
		c.sendAbortTunnel(id, "connect wait: "+err.Error())
		return nil, fmt.Errorf("tunnel connect: %w", err)
	}
	if !resp.OK {
		c.sendAbortTunnel(id, tunnelAckError(resp))
		return nil, fmt.Errorf("tunnel connect failed: %s", tunnelAckError(resp))
	}

	ts := &TunnelStream{
		client:  c,
		ID:      id,
		dataCh:  make(chan []byte, 64),
		closeCh: make(chan struct{}),
	}
	c.tunnelMu.Lock()
	c.tunnelStreams[id] = ts
	c.tunnelMu.Unlock()
	return ts, nil
}

func (c *Client) getTunnelStream(streamID string) *TunnelStream {
	c.tunnelMu.RLock()
	defer c.tunnelMu.RUnlock()
	return c.tunnelStreams[streamID]
}

func (c *Client) takeTunnelStream(streamID string) *TunnelStream {
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()
	ts := c.tunnelStreams[streamID]
	delete(c.tunnelStreams, streamID)
	return ts
}

// Send 发送本地(IOC) 字节到远端(目标)。
func (ts *TunnelStream) Send(p []byte) error {
	if ts.closed.Load() {
		return io.EOF
	}
	return ts.client.sendMessage(&protocol.Message{
		Type:     protocol.MsgTunnelData,
		ID:       uuid.New().String(),
		StreamID: ts.ID,
		Payload:  protocol.TunnelData{StreamID: ts.ID, Data: p},
	})
}

// Recv 阻塞读取远端字节;隧道关闭时返回 io.EOF。
func (ts *TunnelStream) Recv() ([]byte, error) {
	select {
	case d := <-ts.dataCh:
		if d == nil {
			return nil, io.EOF
		}
		return d, nil
	case <-ts.closeCh:
		return nil, io.EOF
	}
}

// deliver 投递一条远端字节(非阻塞,消费慢则丢弃)。
func (ts *TunnelStream) deliver(data []byte) {
	if ts.closed.Load() {
		return
	}
	select {
	case ts.dataCh <- data:
	default:
	}
}

// close 置已关闭,唤醒 Recv。(由远端 MsgTunnelEnd 或本地主动 Close 触发,不重复关闭通道)
func (ts *TunnelStream) close() {
	if ts.closed.Swap(true) {
		return
	}
	ts.once.Do(func() { close(ts.closeCh) })
	ts.client.takeTunnelStream(ts.ID)
}

// Close 主动关闭:向远端发 MsgTunnelEnd 并停止本地接收。
func (ts *TunnelStream) Close() {
	ts.close()
	_ = ts.client.sendMessage(&protocol.Message{
		Type:     protocol.MsgTunnelEnd,
		ID:       uuid.New().String(),
		StreamID: ts.ID,
		Payload:  protocol.TunnelEnd{StreamID: ts.ID, Reason: "local closed"},
	})
}

// sendAbortTunnel 在建连失败/放弃时对该 stream 发 MsgTunnelEnd,让中转拆除注册表条目,
// 并把关闭通知给(可能已经 accept 的)执行方。对从未注册的 stream 发是无害的空操作。
func (c *Client) sendAbortTunnel(streamID, reason string) {
	_ = c.sendMessage(&protocol.Message{
		Type:     protocol.MsgTunnelEnd,
		ID:       uuid.New().String(),
		StreamID: streamID,
		Payload:  protocol.TunnelEnd{StreamID: streamID, Reason: reason},
	})
}

func tunnelAckError(resp *protocol.Response) string {
	if resp.Error != "" {
		return resp.Error
	}
	if resp.Payload == nil {
		return "unknown error"
	}
	if s, ok := resp.Payload.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", resp.Payload)
}
