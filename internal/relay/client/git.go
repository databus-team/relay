package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

// GitStatusSession 是一次入站 Git 状态查询的执行句柄 (执行方视角)
type GitStatusSession struct {
	client    *Client
	requestID string
	req       protocol.GitStatusRequest
}

func (s *GitStatusSession) Workspace() string { return s.req.WorkspaceID }
func (s *GitStatusSession) Dir() string       { return s.req.WorkspaceDir }

func (s *GitStatusSession) Done(resp *protocol.GitStatusResponse, err error) error {
	if err != nil {
		return s.client.sendMessage(&protocol.Message{
			Type:      protocol.MsgError,
			RequestID: s.requestID,
			Payload:   err.Error(),
		})
	}
	return s.client.sendMessage(&protocol.Message{
		Type:      protocol.MsgResponse,
		RequestID: s.requestID,
		ID:        uuid.New().String(),
		Payload:   resp,
	})
}

// GitUndoSession 是一次入站 Git 快照恢复的执行句柄 (执行方视角)
type GitUndoSession struct {
	client    *Client
	requestID string
	req       protocol.GitUndoRequest
}

func (s *GitUndoSession) Workspace() string { return s.req.WorkspaceID }
func (s *GitUndoSession) Dir() string       { return s.req.WorkspaceDir }
func (s *GitUndoSession) TargetRef() string { return s.req.TargetRef }

func (s *GitUndoSession) Done(resp *protocol.GitUndoResponse, err error) error {
	if err != nil {
		return s.client.sendMessage(&protocol.Message{
			Type:      protocol.MsgError,
			RequestID: s.requestID,
			Payload:   err.Error(),
		})
	}
	return s.client.sendMessage(&protocol.Message{
		Type:      protocol.MsgResponse,
		RequestID: s.requestID,
		ID:        uuid.New().String(),
		Payload:   resp,
	})
}

var (
	gitHandlerMu sync.RWMutex
	gitStatusH   func(*GitStatusSession)
	gitUndoH     func(*GitUndoSession)
)

// SetGitStatusHandler 设置入站 git-status 回调 (执行方视角)
func (c *Client) SetGitStatusHandler(fn func(*GitStatusSession)) {
	gitHandlerMu.Lock()
	gitStatusH = fn
	gitHandlerMu.Unlock()
}

// SetGitUndoHandler 设置入站 git-undo 回调 (执行方视角)
func (c *Client) SetGitUndoHandler(fn func(*GitUndoSession)) {
	gitHandlerMu.Lock()
	gitUndoH = fn
	gitHandlerMu.Unlock()
}

func (c *Client) handleInboundGitStatus(msg protocol.Message) {
	data, _ := json.Marshal(msg.Payload)
	var req protocol.GitStatusRequest
	_ = json.Unmarshal(data, &req)

	gitHandlerMu.RLock()
	h := gitStatusH
	gitHandlerMu.RUnlock()

	sess := &GitStatusSession{client: c, requestID: msg.ID, req: req}
	if h == nil {
		_ = sess.Done(nil, fmt.Errorf("git status handler not configured on executor"))
		return
	}
	go h(sess)
}

func (c *Client) handleInboundGitUndo(msg protocol.Message) {
	data, _ := json.Marshal(msg.Payload)
	var req protocol.GitUndoRequest
	_ = json.Unmarshal(data, &req)

	gitHandlerMu.RLock()
	h := gitUndoH
	gitHandlerMu.RUnlock()

	sess := &GitUndoSession{client: c, requestID: msg.ID, req: req}
	if h == nil {
		_ = sess.Done(nil, fmt.Errorf("git undo handler not configured on executor"))
		return
	}
	go h(sess)
}

// GitStatus 向目标节点请求查询 Git 状态
func (c *Client) GitStatus(ctx context.Context, targetExecutor, workspace string) (*protocol.GitStatusResponse, error) {
	resp, err := c.Request(ctx, protocol.MsgGitStatus, protocol.GitStatusRequest{
		ExecutorID:  targetExecutor,
		WorkspaceID: workspace,
	})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("git status failed: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Payload)
	var gsResp protocol.GitStatusResponse
	if err := json.Unmarshal(data, &gsResp); err != nil {
		return nil, err
	}
	return &gsResp, nil
}

// GitUndo 向目标节点请求恢复影子快照
func (c *Client) GitUndo(ctx context.Context, targetExecutor, workspace, targetRef string) (*protocol.GitUndoResponse, error) {
	resp, err := c.Request(ctx, protocol.MsgGitUndo, protocol.GitUndoRequest{
		ExecutorID:  targetExecutor,
		WorkspaceID: workspace,
		TargetRef:   targetRef,
	})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("git undo failed: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Payload)
	var guResp protocol.GitUndoResponse
	if err := json.Unmarshal(data, &guResp); err != nil {
		return nil, err
	}
	return &guResp, nil
}
