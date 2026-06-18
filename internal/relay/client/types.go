package client

import (
	"time"

	"github.com/user/relay/internal/relay/protocol"
)

type Message = protocol.Message

// sendMsg wraps a protocol.Message with optional binary data.
// When Raw is non-nil, the writeLoop sends the JSON message first,
// then the raw bytes as a WebSocket binary frame.
type sendMsg struct {
	*protocol.Message
	Raw []byte
}

func timeNow() int64 {
	return time.Now().UnixMilli()
}
