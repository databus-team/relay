package protocol

import (
	"encoding/json"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{
			name: "connect",
			msg: Message{
				Type: MsgConnect,
				ID:   "test-id",
				Payload: ConnectRequest{
					ClientID:  "client-1",
					Token:     "secret",
					Version:   1,
					Subscribe: []string{"watch-1"},
				},
			},
		},
		{
			name: "connect_ack",
			msg: Message{
				Type:      MsgConnectAck,
				ID:        "ack-id",
				RequestID: "test-id",
				Payload: ConnectResponse{
					OK:            true,
					ServerID:      "srv-1",
					ServerVersion: 1,
					WatchDirs:     []string{"watch-1"},
				},
			},
		},
		{
			name: "file_event",
			msg: Message{
				Type: MsgFileEvent,
				ID:   "event-id",
				Payload: FileEvent{
					EventID: "event-id",
					WatchID: "watch-1",
					Path:    "test.txt",
					Name:    "test.txt",
					Dir:     ".",
					Op:      OpCreate,
					Size:    1024,
					ModTime: 1234567890,
				},
			},
		},
		{
			name: "exec_request",
			msg: Message{
				Type: MsgExec,
				ID:   "exec-id",
				Payload: ExecRequest{
					ExecutorID: "watch-1",
					Cmd:        "ls -la",
					Cwd:        "/tmp",
					Timeout:    30,
				},
			},
		},
		{
			name: "exec_response",
			msg: Message{
				Type:      MsgResponse,
				ID:        "resp-id",
				RequestID: "exec-id",
				Payload: ExecResponse{
					ExitCode: 0,
					Stdout:   "hello",
					Stderr:   "",
					Duration: 100,
				},
			},
		},
		{
			name: "push_request",
			msg: Message{
				Type:     MsgPush,
				ID:       "push-id",
				StreamID: "stream-1",
				Payload: PushRequest{
					WatchID:  "watch-1",
					Path:     "file.bin",
					Size:     4096,
					StreamID: "stream-1",
				},
			},
		},
		{
			name: "pull_request",
			msg: Message{
				Type: MsgPull,
				ID:   "pull-id",
				Payload: PullRequest{
					WatchID: "watch-1",
					Path:    "file.bin",
					Offset:  0,
				},
			},
		},
		{
			name: "server_upgrade",
			msg: Message{
				Type:     MsgServerUpgrade,
				ID:       "sup-id",
				StreamID: "stream-1",
				Payload: ServerUpgradeRequest{
					ExecutorID: "watch-1",
					Size:       4096,
					Digest:     "abcdef0123456789",
					StreamID:   "stream-1",
				},
			},
		},
		{
			name: "tunnel_connect",
			msg: Message{
				Type:     MsgTunnelConnect,
				ID:       "tunnel-id-1",
				StreamID: "tunnel-id-1",
				Payload: TunnelConnectRequest{
					ExecutorID: "site-a",
					Target:     "api.internal.com",
					Port:       443,
					StreamID:   "tunnel-id-1",
				},
			},
		},
		{
			name: "tunnel_data",
			msg: Message{
				Type:     MsgTunnelData,
				ID:       "tun-data",
				StreamID: "tunnel-id-1",
				Payload: TunnelData{
					StreamID: "tunnel-id-1",
					Data:     []byte("hello-bytes"),
				},
			},
		},
		{
			name: "tunnel_end",
			msg: Message{
				Type:     MsgTunnelEnd,
				ID:       "tun-end",
				StreamID: "tunnel-id-1",
				Payload: TunnelEnd{
					StreamID: "tunnel-id-1",
					Reason:   "closed",
				},
			},
		},
		{
			name: "stream_start",
			msg: Message{
				Type:     MsgStreamStart,
				ID:       "ss-id",
				StreamID: "stream-1",
				Payload: StreamStart{
					StreamID:  "stream-1",
					Total:     4096,
					Offset:    0,
					Remaining: 4096,
					Digest:    "abc123",
				},
			},
		},
		{
			name: "stream_end",
			msg: Message{
				Type:     MsgStreamEnd,
				ID:       "se-id",
				StreamID: "stream-1",
				Payload: StreamEnd{
					StreamID: "stream-1",
					OK:       true,
					Received: 4096,
					Digest:   "abc123",
				},
			},
		},
		{
			name: "subscribe",
			msg: Message{
				Type: MsgSubscribe,
				ID:   "sub-id",
				Payload: SubscribeRequest{
					WatchID: "watch-1",
					Action:  "add",
				},
			},
		},
		{
			name: "subscribed_response",
			msg: Message{
				Type:      MsgResponse,
				ID:        "resp-id",
				RequestID: "sub-id",
				Payload: SubscribedResponse{
					WatchID: "watch-1",
					OK:      true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got Message
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got.Type != tt.msg.Type {
				t.Errorf("type: got %q, want %q", got.Type, tt.msg.Type)
			}
			if got.ID != tt.msg.ID {
				t.Errorf("id: got %q, want %q", got.ID, tt.msg.ID)
			}
			if got.RequestID != tt.msg.RequestID {
				t.Errorf("request_id: got %q, want %q", got.RequestID, tt.msg.RequestID)
			}
			if got.StreamID != tt.msg.StreamID {
				t.Errorf("stream_id: got %q, want %q", got.StreamID, tt.msg.StreamID)
			}
		})
	}
}

func TestServerUpgradeRequestRoundTrip(t *testing.T) {
	in := ServerUpgradeRequest{
		ExecutorID: "watch-1",
		Size:       4096,
		Digest:     "abcdef0123456789",
		StreamID:   "stream-1",
	}

	msg := Message{Type: MsgServerUpgrade, ID: "sup-id", StreamID: in.StreamID, Payload: in}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	raw, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p ServerUpgradeRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if p.ExecutorID != in.ExecutorID || p.Size != in.Size || p.Digest != in.Digest || p.StreamID != in.StreamID {
		t.Errorf("payload mismatch: got %+v, want %+v", p, in)
	}
}

func TestTunnelConnectRequestRoundTrip(t *testing.T) {
	in := TunnelConnectRequest{
		ExecutorID: "site-a",
		Target:     "10.0.0.5",
		Port:       8080,
		StreamID:   "tunnel-9",
	}

	msg := Message{Type: MsgTunnelConnect, ID: in.StreamID, StreamID: in.StreamID, Payload: in}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	raw, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p TunnelConnectRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if p.ExecutorID != in.ExecutorID || p.Target != in.Target || p.Port != in.Port || p.StreamID != in.StreamID {
		t.Errorf("payload mismatch: got %+v, want %+v", p, in)
	}
}

// StatusRequest 的可选收窄字段(只探部分出口 + 单次探针预算)必须原样过线,
// 否则中转会退回全网体检,`relay ps` 又被无关的慢执行方拖住。
func TestStatusRequestRoundTrip(t *testing.T) {
	in := Message{
		Type: MsgStatus,
		ID:   "status-id",
		Payload: StatusRequest{
			WithUsage:      true,
			ExecutorIDs:    []string{"eg-a", "eg-b"},
			ProbeTimeoutMS: 800,
		},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p StatusRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if !p.WithUsage {
		t.Error("with_usage: want true")
	}
	if len(p.ExecutorIDs) != 2 || p.ExecutorIDs[0] != "eg-a" || p.ExecutorIDs[1] != "eg-b" {
		t.Errorf("executor_ids: got %v, want [eg-a eg-b]", p.ExecutorIDs)
	}
	if p.ProbeTimeoutMS != 800 {
		t.Errorf("probe_timeout_ms: got %d, want 800", p.ProbeTimeoutMS)
	}
}

// 省略收窄字段时不产生额外键:老请求方与新中转的线上形态保持不变。
func TestStatusRequestOmitsNarrowingFields(t *testing.T) {
	data, err := json.Marshal(StatusRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("empty StatusRequest = %s, want {}", data)
	}
}

func TestStatusResponseRoundTrip(t *testing.T) {
	lat := int64(42)
	in := Message{
		Type:      MsgStatus,
		ID:        "status-id",
		RequestID: "req-1",
		Payload: StatusResponse{
			OK: true,
			Seg1: StatusSegment{
				LatencyMS: &lat,
			},
			Transit: VersionInfo{Role: "transit", Version: "1.0", GOOS: "linux", GOARCH: "amd64"},
			Executors: []ExecutorHealth{
				{
					ExecutorID: "watch-1",
					Version:    "1.0",
					Seg2:       StatusSegment{LatencyMS: &lat},
					Total:      StatusSegment{LatencyMS: &lat},
				},
			},
		},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != MsgStatus {
		t.Errorf("type: got %q, want %q", got.Type, MsgStatus)
	}
	if got.RequestID != "req-1" {
		t.Errorf("request_id: got %q, want %q", got.RequestID, "req-1")
	}

	raw, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p StatusResponse
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if !p.OK {
		t.Error("ok: want true")
	}
	if p.Seg1.LatencyMS == nil || *p.Seg1.LatencyMS != lat {
		t.Errorf("seg1.latency_ms: got %v, want %d", p.Seg1.LatencyMS, lat)
	}
	if p.Transit.Role != "transit" || p.Transit.Version != "1.0" {
		t.Errorf("transit mismatch: got %+v", p.Transit)
	}
	if len(p.Executors) != 1 {
		t.Fatalf("executors: got %d, want 1", len(p.Executors))
	}
	e := p.Executors[0]
	if e.ExecutorID != "watch-1" || e.Version != "1.0" {
		t.Errorf("executor headers mismatch: got %+v", e)
	}
	if e.Seg2.LatencyMS == nil || *e.Seg2.LatencyMS != lat {
		t.Errorf("executor seg2.latency_ms: got %v, want %d", e.Seg2.LatencyMS, lat)
	}
	if e.Total.LatencyMS == nil || *e.Total.LatencyMS != lat {
		t.Errorf("executor total.latency_ms: got %v, want %d", e.Total.LatencyMS, lat)
	}
}

func TestStatusSegmentNullLatencySerializesEmpty(t *testing.T) {
	// 段不可用时 LatencyMS 为 nil:latency_ms 不应出现在 JSON 中(匹配 R2/AE2 "字段留空")。
	seg := StatusSegment{Unavailable: "executor offline"}
	data, err := json.Marshal(seg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("marshaled struct should not be empty")
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["latency_ms"]; ok {
		t.Errorf("latency_ms should be absent when nil, got %+v", got)
	}
	if got["unavailable"] != "executor offline" {
		t.Errorf("unavailable: got %v, want %q", got["unavailable"], "executor offline")
	}
}

func TestServerUpgradeRequestNullPayload(t *testing.T) {
	var req ServerUpgradeRequest
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) == "" {
		t.Fatal("marshaled null struct should not be empty")
	}
	var got ServerUpgradeRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExecutorID != "" || got.Size != 0 || got.Digest != "" || got.StreamID != "" {
		t.Errorf("zero-value mismatch: got %+v", got)
	}
}

func TestStreamDataBinaryContent(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}

	sd := StreamData{
		StreamID: "stream-1",
		Offset:   0,
		Data:     data,
		Chunk:    0,
	}

	encoded, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got StreamData
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Data) != len(data) {
		t.Fatalf("data length: got %d, want %d", len(got.Data), len(data))
	}
	for i := range data {
		if got.Data[i] != data[i] {
			t.Errorf("byte %d: got %d, want %d", i, got.Data[i], data[i])
		}
	}
}

func TestConstants(t *testing.T) {
	if DefaultChunkSize != 65536 {
		t.Errorf("DefaultChunkSize = %d, want 65536", DefaultChunkSize)
	}
	if DefaultMaxInflight != 16 {
		t.Errorf("DefaultMaxInflight = %d, want 16", DefaultMaxInflight)
	}
}

func TestTunnelBinaryRoundTrip(t *testing.T) {
	streamID := "550e8400-e29b-41d4-a716-446655440000"
	payload := []byte("hello-tunnel-raw-binary-data\x00\xff\xfe")

	encoded := EncodeTunnelBinary(streamID, payload)
	if encoded[0] != TunnelBinaryMagic {
		t.Fatalf("expected magic %x, got %x", TunnelBinaryMagic, encoded[0])
	}
	if int(encoded[1]) != len(streamID) {
		t.Fatalf("expected streamID len %d, got %d", len(streamID), encoded[1])
	}

	gotID, gotPayload, ok := DecodeTunnelBinary(encoded)
	if !ok {
		t.Fatalf("DecodeTunnelBinary returned ok=false")
	}
	if gotID != streamID {
		t.Errorf("streamID mismatch: got %q, want %q", gotID, streamID)
	}
	if string(gotPayload) != string(payload) {
		t.Errorf("payload mismatch: got %v, want %v", gotPayload, payload)
	}
}

func TestTunnelBinaryEmptyPayload(t *testing.T) {
	streamID := "test-id"
	encoded := EncodeTunnelBinary(streamID, nil)
	gotID, gotPayload, ok := DecodeTunnelBinary(encoded)
	if !ok {
		t.Fatalf("DecodeTunnelBinary returned ok=false")
	}
	if gotID != streamID {
		t.Errorf("streamID mismatch: got %q, want %q", gotID, streamID)
	}
	if len(gotPayload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(gotPayload))
	}
}

func TestTunnelBinaryCorrupt(t *testing.T) {
	// 长度不足 2
	if _, _, ok := DecodeTunnelBinary([]byte{TunnelBinaryMagic}); ok {
		t.Errorf("expected ok=false for 1-byte data")
	}
	// 错误的 magic
	if _, _, ok := DecodeTunnelBinary([]byte{0x00, 0x01, 'a'}); ok {
		t.Errorf("expected ok=false for wrong magic")
	}
	// 长度字段大于剩余数据
	if _, _, ok := DecodeTunnelBinary([]byte{TunnelBinaryMagic, 10, 'a'}); ok {
		t.Errorf("expected ok=false for truncated streamID")
	}
}

