package client

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

const (
	// heartbeatInterval 心跳间隔:每此间隔向中转发一条 MsgPing 维持活性,并借 pong 刷新
	// 连接的读 deadline(见 Client.readLoop)。NAT/代理后的连接不会立刻拿到 EOF,靠它探测。
	heartbeatInterval = 20 * time.Second
	// heartbeatTimeout 视为失联的 pong 最大计龄。真正的死连接会先在 readLoop 被读
	// deadline(见 readDeadline)更早捕获并重连,此值为兜底。
	heartbeatTimeout = 60 * time.Second
)

func (c *Client) startHeartbeat(ctx context.Context) {
	// 每次进入接管当前连接的「新一代」心跳;重连成功后 reconnectLoop 会再次
	// startHeartbeat 递增 heartbeatGen,使旧代际在下一个 tick 检测到被取代即自行退出,
	// 避免同一连接上叠叠两套心跳 goroutine。
	gen := c.heartbeatGen.Add(1)
	ticker := time.NewTicker(heartbeatInterval)
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

			if now-lastPong > int64(heartbeatTimeout.Milliseconds()) {
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
