package backend

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
)

// fakeExecutor 是一个最小的执行方:只做「连接 + 注册」,之后按 pong 决定是否应答中转探针。
// 一个在线却不应答探针的执行方,正是 `relay ps` 慢的真实成因(执行方发送队列被隧道流量打满时,
// 探针回包被丢弃),这里用它把这个退化场景钉进测试。
type fakeExecutor struct {
	conn *websocket.Conn
}

func (f *fakeExecutor) Close() { _ = f.conn.Close() }

func startFakeExecutor(t testing.TB, wsURL, executorID string, pong bool) *fakeExecutor {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial fake executor: %v", err)
	}
	f := &fakeExecutor{conn: conn}
	t.Cleanup(f.Close)

	if err := conn.WriteJSON(&protocol.Message{
		Type: protocol.MsgConnect,
		ID:   uuid.New().String(),
		Payload: protocol.ConnectRequest{
			ClientID: uuid.New().String(),
			Token:    "tok-exe",
			Version:  1,
		},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	var ack protocol.Message
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("connect ack: %v", err)
	}
	if ack.Type != protocol.MsgConnectAck {
		t.Fatalf("connect ack: got %q", ack.Type)
	}

	regID := uuid.New().String()
	if err := conn.WriteJSON(&protocol.Message{
		Type: protocol.MsgRegisterExecutor,
		ID:   regID,
		Payload: protocol.RegisterExecutorRequest{
			ExecutorID: executorID,
			Action:     "add",
			Version:    "test",
			OS:         "linux",
			Arch:       "amd64",
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	regDone := make(chan struct{})
	go func() {
		for {
			var msg protocol.Message
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case protocol.MsgResponse:
				if msg.RequestID == regID {
					close(regDone)
				}
			case protocol.MsgPing:
				if pong {
					_ = conn.WriteJSON(&protocol.Message{
						Type:      protocol.MsgPong,
						ID:        uuid.New().String(),
						RequestID: msg.ID,
					})
				}
			}
		}
	}()

	select {
	case <-regDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("executor %s did not register", executorID)
	}
	return f
}

// newStatusProbeFixture 起一个中转 + 若干执行方,返回请求方后端。
// 布局:eg-fast(应答探针)、other-0..3(应答探针)、eg-hung(在线但不应答探针)。
func newStatusProbeFixture(t testing.TB) *RelayBackend {
	t.Helper()
	t.Cleanup(client.CloseAll)

	ts, wsURL := newTestHub(t, t.TempDir())
	t.Cleanup(ts.Close)

	startFakeExecutor(t, wsURL, "eg-fast", true)
	for i := 0; i < 4; i++ {
		startFakeExecutor(t, wsURL, "other-"+string(rune('0'+i)), true)
	}
	startFakeExecutor(t, wsURL, "eg-hung", false)

	rb, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "cli", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	return rb.(*RelayBackend)
}

// 指定 executor_ids 时,中转只探这批出口:无关的慢执行方不该再把应答拖住。
func TestStatusTargeted_ProbesOnlyRequestedExecutors(t *testing.T) {
	rb := newStatusProbeFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	st, err := rb.StatusTargeted(ctx, false, []string{"eg-fast"}, 0)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Executors) != 1 || st.Executors[0].ExecutorID != "eg-fast" {
		t.Fatalf("executors = %+v, want only eg-fast", st.Executors)
	}
	if st.Executors[0].Seg2.LatencyMS == nil {
		t.Errorf("eg-fast seg2 missing: %+v", st.Executors[0].Seg2)
	}
	// 未筛选中转会把 eg-hung 一起探掉,应答要等满 statusProbeTimeout(5s)。
	if elapsed > 3*time.Second {
		t.Errorf("targeted status took %v; the unresponsive executor was probed anyway", elapsed)
	}
}

// 出口自己就是慢的那个时,短探针预算要让调用方拿到及时的 notready,而不是等成本地上限后
// 退化成 unknown。
func TestStatusTargeted_UnresponsiveEgressIsNotReady(t *testing.T) {
	rb := newStatusProbeFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	st, err := rb.StatusTargeted(ctx, false, []string{"eg-hung"}, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Executors) != 1 || st.Executors[0].ExecutorID != "eg-hung" {
		t.Fatalf("executors = %+v, want only eg-hung", st.Executors)
	}
	if got := st.Executors[0].Seg2.Unavailable; got != "executor probe timeout" {
		t.Errorf("seg2.unavailable = %q, want probe timeout", got)
	}
	if elapsed > 3*time.Second {
		t.Errorf("bounded probe took %v, want it bounded by the requested budget", elapsed)
	}
}

// 请求的出口只在台账里(已离线)时也要如实返回,否则隧道行会退化成 unknown 而不是 offline。
func TestStatusTargeted_OfflineEgressComesFromLedger(t *testing.T) {
	t.Cleanup(client.CloseAll)

	ts, wsURL := newTestHub(t, t.TempDir())
	t.Cleanup(ts.Close)

	gone := startFakeExecutor(t, wsURL, "eg-gone", true)
	gone.Close()

	rb, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "cli", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	rbb := rb.(*RelayBackend)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := rbb.StatusTargeted(ctx, false, []string{"eg-gone"}, 0)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if len(st.Executors) == 1 && !st.Executors[0].Online {
			if st.Executors[0].LastSeen == 0 {
				t.Errorf("offline entry has no last_seen: %+v", st.Executors[0])
			}
			if st.Executors[0].Seg2.Unavailable != "executor offline" {
				t.Errorf("seg2.unavailable = %q, want offline", st.Executors[0].Seg2.Unavailable)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("executor never showed up as offline: %+v", st.Executors)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 不带筛选时行为不变:仍是整座部署的体检(`relay nodes` 走的就是这条)。
func TestStatusTargeted_NoFilterStillProbesAll(t *testing.T) {
	rb := newStatusProbeFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := rb.StatusTargeted(ctx, false, nil, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Executors) != 6 {
		t.Fatalf("executors = %d, want 6 (all registered)", len(st.Executors))
	}
	var hung *protocol.ExecutorHealth
	for i := range st.Executors {
		if st.Executors[i].ExecutorID == "eg-hung" {
			hung = &st.Executors[i]
		}
	}
	if hung == nil {
		t.Fatalf("eg-hung missing from unfiltered status: %+v", st.Executors)
	}
	if hung.Seg2.LatencyMS != nil {
		t.Errorf("eg-hung should be reported unresponsive, got %+v", hung.Seg2)
	}
}
