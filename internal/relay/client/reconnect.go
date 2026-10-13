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
	MaxRetries   int // <= 0 表示无限重试(常驻节点默认持续退避重连,永不放弃)
}

func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		Enabled:      true,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		MaxRetries:   0, // 默认持续退避重连,保证常驻 peer node 在网络或中转恢复后自动上线
	}
}

func (c *Client) reconnectLoop(ctx context.Context) {
	if !c.reconnectCfg.Enabled {
		c.markClosed()
		return
	}

	// 保证同一时刻只有一个 reconnectLoop 在进行,避免并发触发时竞争或重叠重试
	if c.reconnecting.Swap(true) {
		return
	}
	defer c.reconnecting.Store(false)

	delay := c.reconnectCfg.InitialDelay
	retries := 0

	for c.reconnectCfg.MaxRetries <= 0 || retries < c.reconnectCfg.MaxRetries {
		select {
		case <-ctx.Done():
			c.markClosed()
			return
		case <-time.After(delay):
		}

		if c.reconnectCfg.MaxRetries > 0 {
			log.Printf("[relay] reconnect attempt %d/%d", retries+1, c.reconnectCfg.MaxRetries)
		} else {
			log.Printf("[relay] reconnect attempt %d", retries+1)
		}

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
	c.markClosed()
}

