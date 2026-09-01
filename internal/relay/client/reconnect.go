package client

import (
	"context"
	"log"
	"time"
)

type ReconnectConfig struct {
	Enabled      bool
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxRetries   int
}

func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		Enabled:      true,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		MaxRetries:   10,
	}
}

func (c *Client) reconnectLoop(ctx context.Context) {
	if !c.reconnectCfg.Enabled {
		close(c.closeCh)
		return
	}

	delay := c.reconnectCfg.InitialDelay
	retries := 0

	for retries < c.reconnectCfg.MaxRetries {
		select {
		case <-ctx.Done():
			close(c.closeCh)
			return
		case <-time.After(delay):
		}

		log.Printf("[relay] reconnect attempt %d/%d", retries+1, c.reconnectCfg.MaxRetries)

		if err := c.dial(ctx); err != nil {
			log.Printf("[relay] reconnect failed: %v", err)
			retries++
			delay *= 2
			if delay > c.reconnectCfg.MaxDelay {
				delay = c.reconnectCfg.MaxDelay
			}
			continue
		}

		log.Printf("[relay] reconnected successfully")
		c.connected.Store(true)
		// 只为新连接重启 readLoop;writeLoop 与 heartbeat 是单例(Connect 启动一次),
		// 重连不复启——否则两个写 goroutine 并发写同一 conn 会 panic。
		go c.readLoop()
		c.fireOnReconnect()
		return
	}

	log.Printf("[relay] reconnect exhausted after %d retries", retries)
	c.failAllPending("connection lost after max retries")
	close(c.closeCh)
}
