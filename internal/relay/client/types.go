package client

import (
	"time"

	"github.com/user/relay/internal/relay/protocol"
)

type Message = protocol.Message

func timeNow() int64 {
	return time.Now().UnixMilli()
}
