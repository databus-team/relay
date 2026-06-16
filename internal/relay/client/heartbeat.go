package client

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

func (c *Client) startHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	c.lastPong.Store(time.Now().UnixMilli())

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closeCh:
			return
		case <-ticker.C:
			now := time.Now().UnixMilli()
			lastPong := c.lastPong.Load()

			if now-lastPong > 90*1000 {
				log.Printf("[relay] heartbeat timeout (last pong %dms ago), disconnecting", now-lastPong)
				c.Disconnect()
				return
			}

			c.sendCh <- &Message{
				Type: protocol.MsgPing,
				ID:   uuid.New().String(),
				Payload: protocol.Heartbeat{
					Timestamp: now,
				},
			}
		}
	}
}
