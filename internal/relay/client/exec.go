package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

// registerInStream 注册一条在途响应流(请求方视角):分配 reqID、建收包通道并登记到
// execStreams。调用方应在返回前 defer unregisterInStream(reqID) 清理,避免字典泄漏。
func (c *Client) registerInStream() (reqID string, ch chan *protocol.Message) {
	reqID = uuid.New().String()
	ch = make(chan *protocol.Message, 256)
	c.execMu.Lock()
	c.execStreams[reqID] = ch
	c.execMu.Unlock()
	return reqID, ch
}

// unregisterInStream 移除 reqID 对应的在途响应流(请求方视角)。
func (c *Client) unregisterInStream(reqID string) {
	c.execMu.Lock()
	delete(c.execStreams, reqID)
	c.execMu.Unlock()
}

// streamChunkedContent 将 content 以 64KB 分块 + 压缩发帧给远端(与 PushJob/Transport/UpgradeServer
// 共用同一块语义、同一摘要),并以 MsgStreamEnd 收尾。返回 nil 表示已完整调度发送。
func (c *Client) streamChunkedContent(ctx context.Context, streamID string, total int64, digest string, content []byte) error {
	chunkSize := protocol.DefaultChunkSize
	var offset int64
	chunk := 0
	for offset < total {
		end := offset + int64(chunkSize)
		if end > total {
			end = total
		}
		piece := content[offset:end]
		dataMsg := &protocol.Message{
			Type:     protocol.MsgStreamData,
			ID:       uuid.New().String(),
			StreamID: streamID,
			Payload:  protocol.StreamData{StreamID: streamID, Offset: offset, Chunk: chunk},
		}
		select {
		case c.sendCh <- sendMsg{Message: dataMsg, Raw: protocol.Compress(piece)}:
		case <-ctx.Done():
			return ctx.Err()
		}
		offset = end
		chunk++
	}

	endMsg := &protocol.Message{
		Type:     protocol.MsgStreamEnd,
		ID:       uuid.New().String(),
		StreamID: streamID,
		Payload:  protocol.StreamEnd{StreamID: streamID, OK: true, Received: total, Digest: digest},
	}
	select {
	case c.sendCh <- sendMsg{Message: endMsg}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ExecStream 发送命令请求并流式接收输出。onChunk 每次收到增量输出帧时回调(nil 可忽略)。
// 返回最终 ExecResponse(含聚合 stdout/stderr 与 exit code)。
// targetExecutor 若非空则以它作为请求路由目标(目标执行方 executor_id);空则回退到本端身份。
func (c *Client) ExecStream(ctx context.Context, targetExecutor, cmd, cwd string, timeout int, onChunk func(protocol.ExecChunk)) (*protocol.ExecResponse, error) {
	if timeout <= 0 {
		timeout = 30
	}
	if targetExecutor == "" {
		targetExecutor = c.id
	}

	reqID, ch := c.registerInStream()
	defer c.unregisterInStream(reqID)

	msg := &protocol.Message{
		Type: protocol.MsgExec,
		ID:   reqID,
		Payload: protocol.ExecRequest{
			ExecutorID: targetExecutor,
			Cmd:        cmd,
			Cwd:        cwd,
			Timeout:    timeout,
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
	return c.ExecStream(ctx, "", cmd, cwd, timeout, nil)
}

// UpgradeServer 把本地构建的 relay 二进制以流式分块 + sha256 摘要交付给中转(请求方视角),
// 触发中转「服务器自升级」。本方法以中转自检通过后的成功 ACK 为结算点返回 nil;此后中转
// 才停旧/换装/重启,断线重连与 `relay version -r` 的最终核验由上层 CLI 负责。
func (c *Client) UpgradeServer(ctx context.Context, content []byte) error {
	if len(content) == 0 {
		content = []byte{}
	}

	reqID, ch := c.registerInStream()
	defer c.unregisterInStream(reqID)
	streamID := uuid.New().String()

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	total := int64(len(content))

	header := &protocol.Message{
		Type: protocol.MsgServerUpgrade,
		ID:   reqID,
		Payload: protocol.ServerUpgradeRequest{
			ExecutorID: c.id,
			Size:       total,
			Digest:     digest,
			StreamID:   streamID,
		},
	}
	select {
	case c.sendCh <- sendMsg{Message: header}:
	case <-ctx.Done():
		return ctx.Err()
	}

	// 分块流式发送内容(压缩二进制帧),复用与 PushJob/Transport 一致的块语义。
	if err := c.streamChunkedContent(ctx, streamID, total, digest, content); err != nil {
		return err
	}

	// 传输副作用以收尾 ACK(MsgResponse)为结算点;失败(摘要/自检不过)会经 MsgError 返回。
	_, err := c.execRoundTrip(ctx, ch, nil)
	return err
}
func (c *Client) ConfigSync(ctx context.Context, targetExecutor string, payload []byte) (*protocol.ExecResponse, error) {
	reqID, ch := c.registerInStream()
	defer c.unregisterInStream(reqID)

	if targetExecutor == "" {
		targetExecutor = c.id
	}
	msg := &protocol.Message{
		Type: protocol.MsgConfigSync,
		ID:   reqID,
		Payload: protocol.ConfigSyncRequest{
			ExecutorID: targetExecutor,
			Payload:    base64.StdEncoding.EncodeToString(payload),
		},
	}

	select {
	case c.sendCh <- sendMsg{Message: msg}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.execRoundTrip(ctx, ch, nil)
}

// PushJob 将文件内容以 64KB 分块流式直达远端执行方并在其项目目录落地触发流式 jobs。
// 返回最终 ExecResponse(含写入 jobs 输出与 exit code);无推送时由中转兜底暂存。
// target 语义同 targetExecutor(空=单根回退)。
func (c *Client) PushJob(ctx context.Context, targetExecutor, relPath string, content []byte, onChunk func(protocol.ExecChunk)) (*protocol.ExecResponse, error) {
	if targetExecutor == "" {
		targetExecutor = c.id
	}
	if len(content) == 0 {
		content = []byte{}
	}

	reqID, ch := c.registerInStream()
	defer c.unregisterInStream(reqID)
	streamID := uuid.New().String()

	// sha256 digest 供执行方落盘后校验(与 Pull 的校验思路一致)。
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	total := int64(len(content))

	header := &protocol.Message{
		Type: protocol.MsgPushJob,
		ID:   reqID,
		Payload: protocol.PushJobRequest{
			ExecutorID: targetExecutor,
			RelPath:    relPath,
			Size:       total,
			Digest:     digest,
			StreamID:   streamID,
		},
	}

	select {
	case c.sendCh <- sendMsg{Message: header}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 分块流式发送内容(压缩二进制帧),复用经传与 PushJob 一致的块语义。
	if err := c.streamChunkedContent(ctx, streamID, total, digest, content); err != nil {
		return nil, err
	}

	// 传输副作用以收尾 MsgResponse 为结算点:执行方跑完 jobs(或中转暂存)后回执。
	return c.execRoundTrip(ctx, ch, onChunk)
}

// Transport 将 content 以流式纯下发(targetExecutor 上)写到执行端的绝对路径 dest,
// 不触发任何 workspace job(Jobs=false)。返回执行的收尾响应。
func (c *Client) Transport(ctx context.Context, targetExecutor, dest string, content []byte) (*protocol.ExecResponse, error) {
	if len(content) == 0 {
		content = []byte{}
	}

	reqID, ch := c.registerInStream()
	defer c.unregisterInStream(reqID)
	streamID := uuid.New().String()

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	total := int64(len(content))
	jobs := false

	header := &protocol.Message{
		Type: protocol.MsgPushJob,
		ID:   reqID,
		Payload: protocol.PushJobRequest{
			ExecutorID: targetExecutor,
			RelPath:    dest,
			Size:       total,
			Digest:     digest,
			StreamID:   streamID,
			Jobs:       &jobs,
		},
	}

	select {
	case c.sendCh <- sendMsg{Message: header}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := c.streamChunkedContent(ctx, streamID, total, digest, content); err != nil {
		return nil, err
	}

	return c.execRoundTrip(ctx, ch, func(protocol.ExecChunk) {})
}

// PushJobSession 是一次入站 push-job(执行方视角)的写回句柄。
// Temp 是流式内容落地的临时文件;执行方落盘后把最终路径写回 AbsPath。
type PushJobSession struct {
	client     *Client
	requestID  string
	ExecutorID string
	RelPath    string
	Temp       *os.File
	AbsPath    string
	Jobs       bool // true=跑 workspace jobs;false=纯传输(RelPath 为绝对落盘路径)
	seq        atomic.Int64
}

// Write 写回写输出(流回请求方)。
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

// inboundPushReceive 一次入站流式 push 的接收状态(执行方视角)。
type inboundPushReceive struct {
	streamID string
	stream   *PushJobSession
	handler  func(*PushJobSession)
	received int64
	size     int64
	digest   string
}

// handleInboundPushJob 收到流式 push 的元数据头:准备临时接收文件,等待随后的流式内容。
func (c *Client) handleInboundPushJob(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var req protocol.PushJobRequest
	json.Unmarshal(data, &req)

	c.execHandlerM.RLock()
	h := c.pushJobHandler
	c.execHandlerM.RUnlock()
	if h == nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "push job executor not enabled"})
		return
	}
	if req.StreamID == "" {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "push job: missing stream_id"})
		return
	}

	tmp, err := os.CreateTemp("", "relay-pushjob-*")
	if err != nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: msg.ID, Payload: "push job: create temp: " + err.Error()})
		return
	}

	// Jobs 语义:未携带(nil)=跑 jobs;显式 false=纯传输(不跑 job,落盘后即完成)。
	jobs := req.Jobs == nil || (req.Jobs != nil && *req.Jobs)
	sess := &PushJobSession{client: c, requestID: msg.ID, ExecutorID: req.ExecutorID, RelPath: req.RelPath, Temp: tmp, Jobs: jobs}
	pr := &inboundPushReceive{
		streamID: req.StreamID,
		stream:   sess,
		handler:  h,
		size:     req.Size,
		digest:   req.Digest,
	}
	c.pushRecvMu.Lock()
	c.pushRecv[req.StreamID] = pr
	c.pushRecvMu.Unlock()
}

// abort 终止一次失败的入站流式 push:清理临时文件、移除接收状态并回执错误。
func (pr *inboundPushReceive) abort(c *Client, reason string) {
	if pr.stream != nil && pr.stream.Temp != nil {
		name := pr.stream.Temp.Name()
		_ = pr.stream.Temp.Close()
		os.Remove(name)
	}
	c.pushRecvMu.Lock()
	delete(c.pushRecv, pr.streamID)
	c.pushRecvMu.Unlock()
	if pr.stream != nil {
		_ = c.sendMessage(&protocol.Message{Type: protocol.MsgError, RequestID: pr.stream.requestID, Payload: "push job: " + reason})
	}
}

// routeInboundPushStream 请求入站流式 push 的内容帧(命中即消费并返回 true)。
func (c *Client) routeInboundPushStream(msg *protocol.Message) bool {
	if msg.StreamID == "" {
		return false
	}
	c.pushRecvMu.RLock()
	pr := c.pushRecv[msg.StreamID]
	c.pushRecvMu.RUnlock()
	if pr == nil {
		return false
	}
	switch msg.Type {
	case protocol.MsgStreamData:
		c.handleInboundPushData(pr, msg)
	case protocol.MsgStreamEnd:
		c.handleInboundPushEnd(pr, msg)
	}
	return true
}

// handleInboundPushData 追加一段解压后的内容到临时文件,并同时累计摘要。
func (c *Client) handleInboundPushData(pr *inboundPushReceive, msg *protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	var raw []byte
	if b, ok := payload["data"].([]byte); ok {
		raw = b
	}
	chunkData, err := protocol.Decompress(raw)
	if err != nil {
		pr.abort(c, "decompress: "+err.Error())
		return
	}
	pr.received += int64(len(chunkData))
	if pr.size > 0 && pr.received > pr.size {
		pr.abort(c, "received exceeds declared size")
		return
	}
	if _, err := pr.stream.Temp.Write(chunkData); err != nil {
		pr.abort(c, "write temp: "+err.Error())
	}
}

// handleInboundPushEnd 流结束:落盘完成、校验摘要,然后交给 pushJobHandler 在远端跑 jobs。
func (c *Client) handleInboundPushEnd(pr *inboundPushReceive, msg *protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var se protocol.StreamEnd
	json.Unmarshal(data, &se)

	if !se.OK {
		pr.abort(c, "stream error: "+se.Error)
		return
	}

	// 校验接收量/摘要
	if pr.size != pr.received {
		pr.abort(c, fmt.Sprintf("size mismatch: declared %d, received %d", pr.size, pr.received))
		return
	}

	_ = pr.stream.Temp.Close()

	// 移除接收状态(临时文件由执行方 handler 处理完后清理)。
	c.pushRecvMu.Lock()
	delete(c.pushRecv, pr.streamID)
	c.pushRecvMu.Unlock()

	// 完成写入后交由执行方 handler 处理(把内容落到目录并跑 jobs、回流输出)。
	handler := pr.handler
	go handler(pr.stream)
}

// ExecSession 是一次入站 exec(执行方视角)的写回句柄。
type ExecSession struct {
	client    *Client
	requestID string
	req       protocol.ExecRequest
	seq       atomic.Int64
}

// Cmd / Cwd / Timeout / ExecutorID 返回本次入站请求的命令、工作目录、超时秒数与目标执行方。
func (e *ExecSession) Cmd() string        { return e.req.Cmd }
func (e *ExecSession) Cwd() string        { return e.req.Cwd }
func (e *ExecSession) Timeout() int       { return e.req.Timeout }
func (e *ExecSession) ExecutorID() string { return e.req.ExecutorID }

// SetExecHandler 设置入站 exec 处理回调。设置后,该客户端成为可被中转转发 exec 的执行方。
func (c *Client) SetExecHandler(fn func(*ExecSession)) {
	c.execHandlerM.Lock()
	c.execHandler = fn
	c.execHandlerM.Unlock()
}

// handleInboundExec 处理从中转转发来的 exec 请求(执行方视角)。
func (c *Client) handleInboundExec(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	req := protocol.ExecRequest{
		ExecutorID: toString(payload["executor_id"]),
		Cmd:        toString(payload["cmd"]),
		Cwd:        toString(payload["cwd"]),
		Timeout:    int(toFloat64(payload["timeout"])),
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

// ConfigSyncSession 是一次入站 config-sync(执行方视角)的写回句柄。
type ConfigSyncSession struct {
	client    *Client
	requestID string
	req       protocol.ConfigSyncRequest
}

func (s *ConfigSyncSession) ExecutorID() string { return s.req.ExecutorID }
func (s *ConfigSyncSession) Payload() string    { return s.req.Payload }

// SetConfigSyncHandler 设置入站 config-sync 处理回调。与 SetExecHandler/SetPushJobHandler 同构。
func (c *Client) SetConfigSyncHandler(fn func(*ConfigSyncSession)) {
	c.configSyncM.Lock()
	c.configSyncHandler = fn
	c.configSyncM.Unlock()
}

// handleInboundConfigSync 处理从中转转发来的 config-sync 请求(执行方视角)。
func (c *Client) handleInboundConfigSync(msg protocol.Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	req := protocol.ConfigSyncRequest{
		ExecutorID: toString(payload["executor_id"]),
		Payload:    toString(payload["payload"]),
	}

	c.configSyncM.RLock()
	h := c.configSyncHandler
	c.configSyncM.RUnlock()

	sess := &ConfigSyncSession{client: c, requestID: msg.ID, req: req}
	if h == nil {
		_ = sess.Done(protocol.ExecResponse{ExitCode: 1, Stderr: "config-sync executor not enabled"})
		return
	}
	go h(sess)
}

// Done 收尾,返回最终 exit code 与结果。
func (s *ConfigSyncSession) Done(resp protocol.ExecResponse) error {
	return s.client.sendMessage(&protocol.Message{Type: protocol.MsgResponse, RequestID: s.requestID, ID: uuid.New().String(), Payload: resp})
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
