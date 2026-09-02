package client

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

func (c *Client) startHeartbeat(ctx context.Context) {
	// 每次进入接管当前连接的「新一代」心跳;重连成功后 reconnectLoop 会再次
	// startHeartbeat 递增 heartbeatGen,旧代际在下一个 tick 检测到被取代即自行退出,
	// 避免同一连接上叠叠两套心跳 goroutine。
	gen := c.heartbeatGen.Add(1)
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
			if c.heartbeatGen.Load() != gen {
				return // 已被更新的心跳代际取代(重连后的新连接),旧心跳退出
			}
			now := time.Now().UnixMilli()
			lastPong := c.lastPong.Load()

			if now-lastPong > 90*1000 {
				log.Printf("[relay] heartbeat timeout (last pong %dms ago), disconnecting", now-lastPong)
				c.Disconnect()
				return
			}

			c.sendCh <- sendMsg{Message: &Message{
				Type: protocol.MsgPing,
				ID:   uuid.New().String(),
				Payload: protocol.Heartbeat{
					Timestamp: now,
				},
			}}
		}
	}
}
