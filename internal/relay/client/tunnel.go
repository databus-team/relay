package client

import (
	"context"
	"encoding/json"
	"errors"
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

// tunnelWriteQueue 每条入站隧道出站写队列深度。队列是每个隧道独立的,单个慢目标不会挤占别的隧道。
const tunnelWriteQueue = 64

// errTunnelBacklog 入站隧道出站队列满(目标消费太慢)时中止整条隧道的错误。fail-closed:
// 不丢字节、不无限积压,而是拆除并通知,避免在 readLoop 里同步写造成 head-of-line 阻塞。
var errTunnelBacklog = errors.New("tunnel: egress write backlog full, stream aborted")

// tunnelConn 一条入站隧道落到 executor 的真实 TCP 连接。远端发来的字节不经 readLoop 同步写,
// 而是进入该隧道**专职 writer goroutine** 的有界队列:带慢/停读目标的最多阻塞这条隧道自身的
// writer,而不再阻塞 readLoop 处理其它消息(消除 head-of-line)。队列满或写失败都 fail-closed
// 拆除整条隧道。产出自己持有 done/conn,通过 teardown 统一关停(幂等)。
type tunnelConn struct {
	conn     net.Conn
	client   *Client
	streamID string
	wch      chan []byte
	done     chan struct{}
	stopOnce sync.Once
}

// newTunnelConn 建通道并起 write loop。reader(目标→本地)侧由调用方另行 pump。
func newTunnelConn(c *Client, streamID string, conn net.Conn) *tunnelConn {
	tc := &tunnelConn{
		conn:     conn,
		client:   c,
		streamID: streamID,
		wch:      make(chan []byte, tunnelWriteQueue),
		done:     make(chan struct{}),
	}
	go tc.writer()
	return tc
}

// enqueue 把远端字节投入写队列(非阻塞)。队列满 → fail-closed:中止隧道而非丢字节。
func (tc *tunnelConn) enqueue(p []byte) bool {
	select {
	case tc.wch <- p:
		return true
	case <-tc.done:
		return false
	default:
		tc.fail(errTunnelBacklog)
		return false
	}
}

// writer 专属写 goroutine:循环写出出站字节,带单次写上限,写失败即拆除。
func (tc *tunnelConn) writer() {
	for {
		select {
		case p := <-tc.wch:
			_ = tc.conn.SetWriteDeadline(time.Now().Add(tunnelWriteTimeout))
			_, err := tc.conn.Write(p)
			_ = tc.conn.SetWriteDeadline(time.Time{})
			if err != nil {
				tc.fail(fmt.Errorf("target write: %w", err))
				return
			}
		case <-tc.done:
			return
		}
	}
}

// fail 写路径出错:拆除该隧道并通知对端关停,回调 readLoop 不再需要处理该流。
func (tc *tunnelConn) fail(err error) {
	tc.stopOnce.Do(func() {
		close(tc.done)
		tc.client.takeInboundTunnel(tc.streamID) // 关闭 conn,顺带让 reader pump 结束
		_ = tc.client.sendMessage(&protocol.Message{
			Type:     protocol.MsgTunnelEnd,
			ID:       uuid.New().String(),
			StreamID: tc.streamID,
			Payload:  protocol.TunnelEnd{StreamID: tc.streamID, Reason: err.Error()},
		})
	})
}

// teardown 幂等拆除:停止 writer 并关闭底层连接(由取出该隧道条目的调用方在移出 map 后调用)。
func (tc *tunnelConn) teardown() {
	tc.stopOnce.Do(func() {
		close(tc.done)
		tc.conn.Close()
	})
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
	c.tunnelConns[s.streamID] = newTunnelConn(c, s.streamID, conn) // 内含起专职 writer
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

// takeInboundTunnel 移除并关闭一条入站隧道连接(幂等;经 tunnelConn.teardown 停止其 writer)。
func (c *Client) takeInboundTunnel(streamID string) {
	c.tunnelMu.Lock()
	tc := c.tunnelConns[streamID]
	delete(c.tunnelConns, streamID)
	c.tunnelMu.Unlock()
	if tc != nil {
		tc.teardown()
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
			// 只入队到专属 writer;队列满/写失败由 enqueue/winner 幂等 fail-closed 拆隧道并告知远端。
			tc.enqueue(td.Data)
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

// errTunnelBufferOverrun 请求方本地 SOCKS 消费端跟不上远端字节流时中止整条隧道的错误。
// 不再静默丢字节(那会破坏字节流完整性),而是 fail-closed 关闭并让调用方断开连接。
var errTunnelBufferOverrun = errors.New("tunnel: requester buffer overrun, stream aborted")

// tunnelStream 供本地 relay tunnel 使用的一条出站隧道:远端 → 本地的字节走 dataCh;
// 本地 → 远端的字节经 Send 发出。dataCh 是**有界**的:消费端(本地 SOCKS 泵)太慢、
// 队列满时不再丢弃字节,而是 fail-closed 中止整条隧道(errTunnelBufferOverrun),避免
// 字节流静默损坏(开发中继中最隐蔽的一类数据完整性缺陷)。
type TunnelStream struct {
	client  *Client
	ID      string
	dataCh  chan []byte
	errCh   chan error
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
		errCh:   make(chan error, 1),
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

// Recv 阻塞读取远端字节;隧道被中止置错误返回具体错误,正常/半关闭返回 io.EOF。
func (ts *TunnelStream) Recv() ([]byte, error) {
	select {
	case d := <-ts.dataCh:
		if d == nil {
			return nil, io.EOF
		}
		return d, nil
	case err := <-ts.errCh:
		return nil, err
	case <-ts.closeCh:
		return nil, io.EOF
	}
}

// deliver 投递一条远端字节(非阻塞)。消费端太慢、有界队列满时不再静默丢字节:
// fail-closed 中止整条隧道(fail-open 丢字节会破坏 TCP 字节流语义)。
func (ts *TunnelStream) deliver(data []byte) {
	if ts.closed.Load() {
		return
	}
	select {
	case ts.dataCh <- data:
	default:
		ts.abort(errTunnelBufferOverrun)
	}
}

// abort 以指定错误强行中止隧道:置错误唤醒 Recv、置关闭、通知对端拆除。与 close
// 互斥(首次触发者生效),保证调用方收到失败而不是黑盒或无界等待。
func (ts *TunnelStream) abort(err error) {
	if ts.closed.Swap(true) {
		return
	}
	// 只经 errCh 暴露错误于 Rect;不留 closeCh(乃至 Recv 随机跳 EOF,掩盖根因)。
	ts.once.Do(func() {
		select {
		case ts.errCh <- err:
		default:
		}
	})
	_ = ts.client.sendMessage(&protocol.Message{
		Type:     protocol.MsgTunnelEnd,
		ID:       uuid.New().String(),
		StreamID: ts.ID,
		Payload:  protocol.TunnelEnd{StreamID: ts.ID, Reason: err.Error()},
	})
	ts.client.takeTunnelStream(ts.ID)
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

// closeAllTunnels 传输断开/重连时拆空全部隧道。服务端断连已清理它那边的隧道注册表,本地若不
// 拆,出站 requester 流会以为隧道仍开着而静默黑盒(exist,-requester-block)、入站 executor 连接会
// 泄漏。这里统一中止出站流(使 Recv 唤醒)并关闭入站连接(fail-closed,杜绝黑盒)。幂等可重复调用。
func (c *Client) closeAllTunnels(reason string) {
	c.tunnelMu.RLock()
	streams := make([]*TunnelStream, 0, len(c.tunnelStreams))
	for _, ts := range c.tunnelStreams {
		streams = append(streams, ts)
	}
	connStreamIDs := make([]string, 0, len(c.tunnelConns))
	for id := range c.tunnelConns {
		connStreamIDs = append(connStreamIDs, id)
	}
	c.tunnelMu.RUnlock()

	for _, ts := range streams {
		ts.abort(fmt.Errorf("%s", reason))
	}
	for _, id := range connStreamIDs {
		c.takeInboundTunnel(id)
	}
}

// sendAbortTunnel 在建连失败/放弃时对该 stream 发 MsgTunnelEnd,让中转拆除注册表条目,
// 并把关闭通知给(可能已经 accept 的)执行方。对从未注册的 stream 是无害的空操作。
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
