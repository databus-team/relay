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
					WatchID: "watch-1",
					Cmd:     "ls -la",
					Cwd:     "/tmp",
					Timeout: 30,
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
					WatchID:  "watch-1",
					Size:     4096,
					Digest:   "abcdef0123456789",
					StreamID: "stream-1",
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
		WatchID:  "watch-1",
		Size:     4096,
		Digest:   "abcdef0123456789",
		StreamID: "stream-1",
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

	if p.WatchID != in.WatchID || p.Size != in.Size || p.Digest != in.Digest || p.StreamID != in.StreamID {
		t.Errorf("payload mismatch: got %+v, want %+v", p, in)
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
	if got.WatchID != "" || got.Size != 0 || got.Digest != "" || got.StreamID != "" {
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
