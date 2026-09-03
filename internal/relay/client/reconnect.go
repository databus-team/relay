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

		if err := c.establish(ctx); err != nil {
			log.Printf("[relay] reconnect failed: %v", err)
			retries++
			delay *= 2
			if delay > c.reconnectCfg.MaxDelay {
				delay = c.reconnectCfg.MaxDelay
			}
			continue
		}

		log.Printf("[relay] reconnected successfully")
		// establish 已串行化承担 readLoop/heartbeat 的按连接重启,且 writeLoop 用 writeOnce
		// 保证终身只一条(重复 Connect/重连绝不会叠起两个写者并发写同一 conn 导致 panic)。
		c.fireOnReconnect()
		return
	}

	log.Printf("[relay] reconnect exhausted after %d retries", retries)
	c.failAllPending("connection lost after max retries")
	close(c.closeCh)
}
