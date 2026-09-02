package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// 执行方收到中转探针 MsgPing 后应回响 MsgPong、且 RequestID 呼应入站 MsgPing 的 ID。
// 该回包供中转测量 transit→executor 段时延(镜像 server/client.go 的 ping 回包语义)。
func TestExecutorAnswersTransitProbe(t *testing.T) {
	c := &Client{sendCh: make(chan sendMsg, 8)}

	probeID := "probe-1"
	c.handleMessage(protocol.Message{Type: protocol.MsgPing, ID: probeID})

	select {
	case sm := <-c.sendCh:
		if sm.Message == nil {
			t.Fatal("enqueued message is nil")
		}
		if sm.Message.Type != protocol.MsgPong {
			t.Fatalf("reply type: got %q, want %q", sm.Message.Type, protocol.MsgPong)
		}
		if sm.Message.RequestID != probeID {
			t.Errorf("reply request_id: got %q, want %q", sm.Message.RequestID, probeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MsgPong enqueued in response to MsgPing")
	}
}

// 非探针消息(MsgPong 心跳)不应触发执行方回包,仍走原来的 pending 解析逻辑。
func TestExecutorNoReplyForHeartbeatPong(t *testing.T) {
	c := &Client{sendCh: make(chan sendMsg, 8)}
	c.handleMessage(protocol.Message{Type: protocol.MsgPong, ID: "x"})
	select {
	case sm := <-c.sendCh:
		t.Fatalf("unexpected outbound %q for inbound MsgPong", sm.Message.Type)
	case <-time.After(50 * time.Millisecond):
		// ok: no spurious reply
	}
}