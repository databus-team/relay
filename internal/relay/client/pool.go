package client

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

var (
	pool   = make(map[string]*Client)
	poolMu sync.RWMutex
)

// GetOrConnect 获取(或新建并连接)一个客户端。opts 里可带 WithHeaders 等选项。
func GetOrConnect(ctx context.Context, url, token, watchID string, opts ...Option) (*Client, error) {
	key := url + "|" + token + "|" + watchID + "|" + headersKey(getHeader(opts))

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

	c, err := New(url, token, watchID, opts...)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	pool[key] = c
	return c, nil
}

// getHeader 提取 opts 中的 WithHeaders 头(以参与池键)。
func getHeader(opts []Option) http.Header {
	c := &Client{}
	for _, o := range opts {
		o(c)
	}
	return c.headers
}

// headersKey 把 header 确定性序列化,纳入客户端池键看。
func headersKey(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.Join(h[k], ","))
		b.WriteString(";")
	}
	return b.String()
}

func CloseAll() {
	poolMu.Lock()
	defer poolMu.Unlock()

	for key, c := range pool {
		c.Disconnect()
		delete(pool, key)
	}
}
