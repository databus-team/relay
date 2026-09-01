package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

// ExecRequest 是一次远程执行请求(请求方发出或被执行方收到)。
type ExecRequest struct {
	WatchID string
	Cmd     string
	Cwd     string
	Timeout int
}

// ExecStream 发送执行请求并流式接收输出。onChunk 每次收到增量输出帧时回调(nil 可忽略)。
// 返回最终 ExecResponse(含聚合 stdout/stderr 与 exit code)。
func (c *Client) ExecStream(ctx context.Context, cmd, cwd string, timeout int, onChunk func(protocol.ExecChunk)) (*protocol.ExecResponse, error) {
	if timeout <= 0 {
		timeout = 30
	}

	reqID := uuid.New().String()
	ch := make(chan *protocol.Message, 256)

	c.execMu.Lock()
	c.execStreams[reqID] = ch
	c.execMu.Unlock()
	defer func() {
		c.execMu.Lock()
		delete(c.execStreams, reqID)
		c.execMu.Unlock()
	}()

	msg := &protocol.Message{
		Type: protocol.MsgExec,
		ID:   reqID,
		Payload: protocol.ExecRequest{
			WatchID: c.watchID,
			Cmd:     cmd,
			Cwd:     cwd,
			Timeout: timeout,
		},
	}

	select {
	case c.sendCh <- sendMsg{Message: msg}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.execRoundTrip(ctx, ch, onChunk)
}

// execRoundTrip 消费一条在途 exec(或 push_job)的输出帧,直到收尾。
func (c *Client) execRoundTrip(ctx context.Context, ch chan *protocol.Message, onChunk func(protocol.ExecChunk)) (*protocol.ExecResponse, error) {
	var stdout, stderr strings.Builder
	for {
		select {
		case m := <-ch:
			switch m.Type {
			case protocol.MsgExecOutput:
				var chunk protocol.ExecChunk
				data, _ := json.Marshal(m.Payload)
				json.Unmarshal(data, &chunk)
				if chunk.Stdout {
					stdout.WriteString(chunk.Data)
				} else {
					stderr.WriteString(chunk.Data)
				}
				if onChunk != nil {
					onChunk(chunk)
				}
			case protocol.MsgResponse, protocol.MsgError:
				if m.Type == protocol.MsgError {
					var errMsg string
					if s, ok := m.Payload.(string); ok {
						errMsg = s
					}
					return nil, fmt.Errorf("exec failed: %s", errMsg)
				}
				data, _ := json.Marshal(m.Payload)
				var resp protocol.ExecResponse
				json.Unmarshal(data, &resp)
				if resp.Stdout == "" && stdout.Len() > 0 {
					resp.Stdout = stdout.String()
				}
				if resp.Stderr == "" && stderr.Len() > 0 {
					resp.Stderr = stderr.String()
				}
				return &resp, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Exec 缓冲式执行(流式结果聚合后一次性返回),保持向后兼容。
func (c *Client) Exec(ctx context.Context, cmd string, cwd string, timeout int) (*protocol.ExecResponse, error) {
	return c.ExecStream(ctx, cmd, cwd, timeout, nil)
}

// PushJob 将文件内容直达远端执行方,在其项目目录落地并触发 jobs。
// 返回最终 ExecResponse(含 job 输出与 exit code);无在线执行方时由中转转发段兜底。
func (c *Client) PushJob(ctx context.Context, relPath string, content []byte, onChunk func(protocol.ExecChunk)) (*protocol.ExecResponse, error) {
	reqID := uuid.New().String()
	ch := make(chan *protocol.Message, 256)

	c.execMu.Lock()
	c.execStreams[reqID] = ch
	c.execMu.Unlock()
	defer func() {
		c.execMu.Lock()
		delete(c.execStreams, reqID)
		c.execMu.Unlock()
	}()

	msg := &protocol.Message{
		Type: protocol.MsgPushJob,
		ID:   reqID,
		Payload: protocol.PushJobRequest{
			WatchID: c.watchID,
			RelPath: relPath,
			Content: content,
		},
	}

	select {
	case c.sendCh <- sendMsg{Message: msg}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.execRoundTrip(ctx, ch, onChunk)
}

// PushJobSession 是一次入站 push-job(执行方视角)的写回句柄。
type PushJobSession struct {
	client    *Client
	requestID string
	WatchID   string
	RelPath   string
	Content   []byte
	seq       atomic.Int64
}

// Write 写一段增量输出(流回请求方)。
func (p *PushJobSession) Write(stdout bool, data string) error {
	chunk := protocol.ExecChunk{Seq: int(p.seq.Add(1)), Stdout: stdout, Data: data}
	return p.client.sendMessage(&protocol.Message{Type: protocol.MsgExecOutput, RequestID: p.requestID, Payload: chunk})
}

// Done 收尾,返回 job exit code。
func (p *PushJobSession) Done(resp protocol.ExecResponse) error {
	return p.client.sendMessage(&protocol.Message{Type: protocol.MsgResponse, RequestID: p.requestID, ID: uuid.New().String(), Payload: resp})
}

// SetPushJobHandler 设置入站 push-job 回调(执行方角色,配合远端 watch 跑本地 jobs)。
func (c *Client) SetPushJobHandler(fn func(*PushJobSession)) {
	c.execHandlerM.Lock()
	c.pushJobHandler = fn
	c.execHandlerM.Unlock()
}

func (c *Client) handleInboundPushJob(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var req protocol.PushJobRequest
	json.Unmarshal(data, &req)

	c.execHandlerM.RLock()
	h := c.pushJobHandler
	c.execHandlerM.RUnlock()

	sess := &PushJobSession{client: c, requestID: msg.ID, WatchID: req.WatchID, RelPath: req.RelPath, Content: req.Content}
	if h == nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "push job executor not enabled"})
		return
	}
	go h(sess)
}

// ExecSession 是一次入站 exec(执行方视角)的写回句柄。
type ExecSession struct {
	client    *Client
	requestID string
	req       ExecRequest
	seq       atomic.Int64
}

// Cmd / Cwd / Timeout 返回本次入站请求的命令、工作目录与超时秒数。
func (e *ExecSession) Cmd() string  { return e.req.Cmd }
func (e *ExecSession) Cwd() string  { return e.req.Cwd }
func (e *ExecSession) Timeout() int { return e.req.Timeout }

// SetExecHandler 设置入站 exec 处理回调。设置后,该客户端成为可被中转转发 exec 的执行方。
func (c *Client) SetExecHandler(fn func(*ExecSession)) {
	c.execHandlerM.Lock()
	c.execHandler = fn
	c.execHandlerM.Unlock()
}

// handleInboundExec 处理从中转转发来的 exec 请求(执行方视角)。
func (c *Client) handleInboundExec(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	req := ExecRequest{
		WatchID: toString(payload["watch_id"]),
		Cmd:     toString(payload["cmd"]),
		Cwd:     toString(payload["cwd"]),
		Timeout: int(toFloat64(payload["timeout"])),
	}
	if req.Timeout <= 0 {
		req.Timeout = 30
	}

	c.execHandlerM.RLock()
	h := c.execHandler
	c.execHandlerM.RUnlock()

	sess := &ExecSession{client: c, requestID: msg.ID, req: req}
	if h == nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "executor not enabled"})
		return
	}
	go h(sess)
}

// Write 写一行/一段增量输出。
func (e *ExecSession) Write(stdout bool, data string) error {
	chunk := protocol.ExecChunk{Seq: int(e.seq.Add(1)), Stdout: stdout, Data: data}
	return e.client.sendMessage(&protocol.Message{Type: protocol.MsgExecOutput, RequestID: e.requestID, Payload: chunk})
}

// Done 收尾,返回最终 exit code 与聚合输出。
func (e *ExecSession) Done(resp protocol.ExecResponse) error {
	return e.client.sendMessage(&protocol.Message{Type: protocol.MsgResponse, RequestID: e.requestID, ID: uuid.New().String(), Payload: resp})
}

func toFloat64(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// routeExecStream 请求方流式 exec 帧投递。命中在途 exec 即投递并返回 true。
func (c *Client) routeExecStream(msg *protocol.Message) bool {
	if msg.RequestID == "" {
		return false
	}
	c.execMu.RLock()
	ch, ok := c.execStreams[msg.RequestID]
	c.execMu.RUnlock()
	if !ok {
		return false
	}
	// 非阻塞投递;若消费慢则丢弃(与 eventCh 策略一致)
	select {
	case ch <- msg:
	default:
	}
	return true
}
