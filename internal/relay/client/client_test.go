package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

// WithHeaders 提供的自定义头应随 WebSocket 握手发往服务器。
func TestWithHeadersSentOnHandshake(t *testing.T) {
	var got http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var m map[string]interface{}
		_ = conn.ReadJSON(&m) // client Connect
		_ = conn.WriteJSON(protocol.Message{
			Type:    protocol.MsgConnectAck,
			Payload: protocol.ConnectResponse{OK: true},
		})
	}))
	defer srv.Close()
	defer CloseAll()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	h := http.Header{}
	h.Set("X-Auth-Token", "secret123")

	if _, err := GetOrConnect(context.Background(), url, "tok", "w", WithHeaders(h)); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if got.Get("X-Auth-Token") != "secret123" {
		t.Fatalf("X-Auth-Token not sent on handshake; got %v", got)
	}
}