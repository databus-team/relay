package client

import (
	"context"
	"fmt"
	"sync"
)

var (
	pool   = make(map[string]*Client)
	poolMu sync.RWMutex
)

func GetOrConnect(ctx context.Context, url, token, watchID string) (*Client, error) {
	key := url + "|" + token + "|" + watchID

	poolMu.RLock()
	c, ok := pool[key]
	poolMu.RUnlock()
	if ok && c.IsConnected() {
		return c, nil
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	c, ok = pool[key]
	if ok && c.IsConnected() {
		return c, nil
	}

	if ok && !c.IsConnected() {
		c.Disconnect()
	}

	c, err := New(url, token, watchID)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	pool[key] = c
	return c, nil
}

func CloseAll() {
	poolMu.Lock()
	defer poolMu.Unlock()

	for key, c := range pool {
		c.Disconnect()
		delete(pool, key)
	}
}
