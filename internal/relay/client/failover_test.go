package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

func mockHub(t *testing.T, expectedToken string, expectedHeaders map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var connectCount atomic.Int64
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range expectedHeaders {
			if r.Header.Get(k) != v {
				http.Error(w, "forbidden: header mismatch "+k, http.StatusForbidden)
				return
			}
		}

		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var req protocol.Message
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		if req.Type != protocol.MsgConnect {
			return
		}

		var cp protocol.ConnectRequest
		if req.Payload != nil {
			if m, ok := req.Payload.(map[string]interface{}); ok {
				cp.Token, _ = m["token"].(string)
			}
		}

		if cp.Token != expectedToken {
			_ = conn.WriteJSON(protocol.Message{
				Type: protocol.MsgError,
				Payload: "bad token",
			})
			return
		}

		connectCount.Add(1)
		_ = conn.WriteJSON(protocol.Message{
			Type: protocol.MsgConnectAck,
			Payload: protocol.ConnectResponse{
				OK: true,
			},
		})

		// 保持连接读取以响应心跳或等待断开
		for {
			var msg protocol.Message
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == protocol.MsgPing {
				_ = conn.WriteJSON(protocol.Message{
					Type: protocol.MsgPong,
					Payload: protocol.Heartbeat{
						Timestamp: time.Now().UnixMilli(),
					},
				})
			}
		}
	}))

	return srv, &connectCount
}

func TestClient_Dial_PrimarySuccess(t *testing.T) {
	hub1, cnt1 := mockHub(t, "tok-1", map[string]string{"X-Hub": "hub-1"})
	defer hub1.Close()

	hub2, cnt2 := mockHub(t, "tok-2", map[string]string{"X-Hub": "hub-2"})
	defer hub2.Close()

	h1 := http.Header{}
	h1.Set("X-Hub", "hub-1")
	h2 := http.Header{}
	h2.Set("X-Hub", "hub-2")

	endpoints := []Endpoint{
		{URL: "ws" + strings.TrimPrefix(hub1.URL, "http"), Token: "tok-1", Headers: h1},
		{URL: "ws" + strings.TrimPrefix(hub2.URL, "http"), Token: "tok-2", Headers: h2},
	}

	c, err := NewWithEndpoints(endpoints, "test-client")
	if err != nil {
		t.Fatalf("NewWithEndpoints: %v", err)
	}
	defer c.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	if cnt1.Load() != 1 {
		t.Errorf("expected hub1 to receive 1 connection, got %d", cnt1.Load())
	}
	if cnt2.Load() != 0 {
		t.Errorf("expected hub2 to receive 0 connection, got %d", cnt2.Load())
	}
	if c.ActiveURL() != endpoints[0].URL {
		t.Errorf("ActiveURL = %s, want %s", c.ActiveURL(), endpoints[0].URL)
	}
}

func TestClient_Dial_FailoverToStandbyWithDifferentAuth(t *testing.T) {
	// hub1 不启动，直接使用不可达地址
	deadURL := "ws://127.0.0.1:1"

	// hub2 启动并要求独立 token 和 header
	hub2Headers := map[string]string{"Cookie": "sess=xyz", "X-Gateway": "cloud"}
	hub2, cnt2 := mockHub(t, "cloud-token", hub2Headers)
	defer hub2.Close()

	h2 := http.Header{}
	for k, v := range hub2Headers {
		h2.Set(k, v)
	}

	endpoints := []Endpoint{
		{URL: deadURL, Token: "tok-1"},
		{URL: "ws" + strings.TrimPrefix(hub2.URL, "http"), Token: "cloud-token", Headers: h2},
	}

	c, err := NewWithEndpoints(endpoints, "test-client")
	if err != nil {
		t.Fatalf("NewWithEndpoints: %v", err)
	}
	defer c.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	start := time.Now()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	elapsed := time.Since(start)

	if cnt2.Load() != 1 {
		t.Errorf("expected hub2 to receive 1 connection, got %d", cnt2.Load())
	}
	if c.ActiveURL() != endpoints[1].URL {
		t.Errorf("ActiveURL = %s, want %s", c.ActiveURL(), endpoints[1].URL)
	}
	if c.ActiveEndpoint().Token != "cloud-token" {
		t.Errorf("ActiveEndpoint token = %s, want cloud-token", c.ActiveEndpoint().Token)
	}
	t.Logf("failover succeeded in %v", elapsed)
}

func TestClient_Dial_AllFailed(t *testing.T) {
	endpoints := []Endpoint{
		{URL: "ws://127.0.0.1:1", Token: "tok-1"},
		{URL: "ws://127.0.0.1:2", Token: "tok-2"},
	}

	c, err := NewWithEndpoints(endpoints, "test-client")
	if err != nil {
		t.Fatalf("NewWithEndpoints: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = c.Connect(ctx)
	if err == nil {
		c.Disconnect()
		t.Fatalf("expected error when all hubs are down")
	}
	if !strings.Contains(err.Error(), "failed to connect to any hub") {
		t.Errorf("unexpected error format: %v", err)
	}
}

func TestClient_Reconnect_Failover(t *testing.T) {
	hub1, cnt1 := mockHub(t, "tok-1", nil)
	hub2, cnt2 := mockHub(t, "tok-2", nil)
	defer hub2.Close()

	endpoints := []Endpoint{
		{URL: "ws" + strings.TrimPrefix(hub1.URL, "http"), Token: "tok-1"},
		{URL: "ws" + strings.TrimPrefix(hub2.URL, "http"), Token: "tok-2"},
	}

	c, err := NewWithEndpoints(endpoints, "test-client")
	if err != nil {
		t.Fatalf("NewWithEndpoints: %v", err)
	}
	defer c.Disconnect()

	// 快速重连配置用于测试
	c.reconnectCfg = ReconnectConfig{
		Enabled:      true,
		InitialDelay: 50 * time.Millisecond,
		MaxDelay:     200 * time.Millisecond,
		MaxRetries:   5,
	}

	var reconnected atomic.Bool
	c.SetOnReconnect(func() {
		reconnected.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("initial connect failed: %v", err)
	}
	if cnt1.Load() != 1 {
		t.Fatalf("expected hub1 connect count 1, got %d", cnt1.Load())
	}

	// 模拟 Hub1 宕机
	hub1.Close()

	// 触发断开并等待重连漂移到 Hub2
	c.Disconnect()

	// 等待重连成功
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.IsConnected() && c.ActiveURL() == endpoints[1].URL && cnt2.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !c.IsConnected() {
		t.Fatalf("client failed to reconnect")
	}
	if c.ActiveURL() != endpoints[1].URL {
		t.Errorf("ActiveURL = %s, want %s (failover to hub2)", c.ActiveURL(), endpoints[1].URL)
	}
	if cnt2.Load() < 1 {
		t.Errorf("hub2 connect count = %d, want >= 1", cnt2.Load())
	}
	if !reconnected.Load() {
		t.Errorf("onReconnect callback was not fired after failover")
	}
}
