package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

type receiveStream struct {
	streamID   string
	buf        bytes.Buffer
	doneCh     chan struct{}
	errCh      chan error
	digest     string
	total      int64
	compressed bool
	mu         sync.Mutex
}

func (c *Client) Pull(ctx context.Context, path string) ([]byte, error) {
	streamID := uuid.New().String()
	reqID := uuid.New().String()

	rs := &receiveStream{
		streamID: streamID,
		doneCh:   make(chan struct{}),
		errCh:    make(chan error, 1),
	}

	c.streamMu.Lock()
	c.streams[streamID] = rs
	c.streamMu.Unlock()

	defer func() {
		c.streamMu.Lock()
		delete(c.streams, streamID)
		c.streamMu.Unlock()
	}()

	respCh := make(chan *protocol.Response, 1)
	c.pendingMu.Lock()
	c.pending[reqID] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	msg := &Message{
		Type:     protocol.MsgPull,
		ID:       reqID,
		StreamID: streamID,
		Payload: protocol.PullRequest{
			WatchID: c.watchID,
			Path:    path,
			Offset:  0,
		},
	}

	select {
	case c.sendCh <- msg:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case <-rs.doneCh:
		data := rs.buf.Bytes()
		if rs.digest != "" {
			h := sha256.Sum256(data)
			got := hex.EncodeToString(h[:])
			if got != rs.digest {
				return nil, fmt.Errorf("digest mismatch: expected %s, got %s", rs.digest, got)
			}
		}
		return data, nil
	case err := <-rs.errCh:
		return nil, err
	case resp := <-respCh:
		if !resp.OK {
			return nil, fmt.Errorf("pull failed: %s", resp.Error)
		}
		return nil, fmt.Errorf("unexpected response (expected stream)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) Push(ctx context.Context, path string, data []byte) error {
	streamID := uuid.New().String()

	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	total := int64(len(data))

	pushMsg := &Message{
		Type:     protocol.MsgPush,
		ID:       uuid.New().String(),
		StreamID: streamID,
		Payload: protocol.PushRequest{
			WatchID:  c.watchID,
			Path:     path,
			Size:     total,
			StreamID: streamID,
		},
	}

	resp, err := c.sendAndWait(ctx, pushMsg)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("push rejected: %s", resp.Error)
	}

	chunkSize := protocol.DefaultChunkSize
	var offset int64
	chunk := 0
	for offset < total {
		end := offset + int64(chunkSize)
		if end > total {
			end = total
		}

		chunkData := data[offset:end]
		compressed := protocol.Compress(chunkData)

		dataMsg := &Message{
			Type:     protocol.MsgStreamData,
			ID:       uuid.New().String(),
			StreamID: streamID,
			Payload: protocol.StreamData{
				StreamID: streamID,
				Offset:   offset,
				Data:     compressed,
				Chunk:    chunk,
			},
		}

		select {
		case c.sendCh <- dataMsg:
		case <-ctx.Done():
			return ctx.Err()
		}

		offset = end
		chunk++
	}

	endMsg := &Message{
		Type:     protocol.MsgStreamEnd,
		ID:       uuid.New().String(),
		StreamID: streamID,
		Payload: protocol.StreamEnd{
			StreamID: streamID,
			OK:       true,
			Received: total,
			Digest:   digest,
		},
	}

	doneCh := make(chan error, 1)
	c.streamMu.Lock()
	c.streamDone[streamID] = doneCh
	c.streamMu.Unlock()

	select {
	case c.sendCh <- endMsg:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-doneCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) handleStreamStart(msg Message) {
	streamID := msg.StreamID

	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var start protocol.StreamStart
	json.Unmarshal(data, &start)

	c.streamMu.RLock()
	rs, ok := c.streams[streamID]
	c.streamMu.RUnlock()

	if !ok {
		return
	}

	rs.mu.Lock()
	rs.total = start.Total
	rs.compressed = start.Compressed
	rs.mu.Unlock()
}

func (c *Client) handleStreamData(msg Message) {
	streamID := msg.StreamID

	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var sd protocol.StreamData
	json.Unmarshal(data, &sd)

	c.streamMu.RLock()
	rs, ok := c.streams[streamID]
	c.streamMu.RUnlock()

	if !ok {
		return
	}

	chunkData := sd.Data
	if rs.compressed {
		decompressed, err := protocol.Decompress(sd.Data)
		if err != nil {
			select {
			case rs.errCh <- fmt.Errorf("decompress: %w", err):
			default:
			}
			return
		}
		chunkData = decompressed
	}

	rs.mu.Lock()
	rs.buf.Write(chunkData)
	rs.mu.Unlock()
}

func (c *Client) handleStreamEnd(msg Message) {
	streamID := msg.StreamID

	payload, _ := msg.Payload.(map[string]interface{})
	data, _ := json.Marshal(payload)
	var se protocol.StreamEnd
	json.Unmarshal(data, &se)

	c.streamMu.RLock()
	rs, ok := c.streams[streamID]
	c.streamMu.RUnlock()

	if !ok {
		return
	}

	rs.mu.Lock()
	rs.digest = se.Digest
	rs.mu.Unlock()

	if !se.OK {
		select {
		case rs.errCh <- fmt.Errorf("stream error: %s", se.Error):
		default:
		}
		return
	}

	close(rs.doneCh)
}
