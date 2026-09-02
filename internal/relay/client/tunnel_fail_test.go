package client

import (
	"errors"
	"net"
	"testing"

	"github.com/user/relay/internal/relay/protocol"
)

// newTunnelTestClient 构造一个可被隧道逻辑直接操作的最小 Client(测试用)。
func newTunnelTestClient() *Client {
	return &Client{
		sendCh:        make(chan sendMsg, 16),
		tunnelStreams: make(map[string]*TunnelStream),
		tunnelConns:   make(map[string]*tunnelConn),
	}
}

// 请求方数据通道满时应 fail-closed 中止隧道,而不是静默丢字节:Recv 最终返回错误,
// stream 被拆除,并向对端发 MsgTunnelEnd。
func TestTunnelStreamOverrunFailsClosed(t *testing.T) {
	c := newTunnelTestClient()
	ts := &TunnelStream{
		client:  c,
		ID:      "t1",
		dataCh:  make(chan []byte, 2),
		errCh:   make(chan error, 1),
		closeCh: make(chan struct{}),
	}
	c.tunnelStreams["t1"] = ts

	ts.deliver([]byte("ab"))
	ts.deliver([]byte("cd")) // dataCh 填满(cap=2)
	ts.deliver([]byte("ef")) // 溢出 → 中止

	var gotErr error
	for i := 0; i <= 5; i++ {
		if _, e := ts.Recv(); e != nil {
			gotErr = e
			break
		}
	}
	if !errors.Is(gotErr, errTunnelBufferOverrun) {
		t.Fatalf("Recv error: %v, want errTunnelBufferOverrun", gotErr)
	}
	if _, ok := c.tunnelStreams["t1"]; ok {
		t.Errorf("overrun stream not removed from registry")
	}
	if sm := <-c.sendCh; sm.Message.Type != protocol.MsgTunnelEnd {
		t.Errorf("expected MsgTunnelEnd on overrun abort, got %q", sm.Message.Type)
	}
}

// 传输断开时 closeAllTunnels 应中止出站流(Recv 报错)、拆除入站连接,不残留黑盒或连接。
func TestCloseAllTunnelsOnTransportLoss(t *testing.T) {
	c := newTunnelTestClient()

	requester := &TunnelStream{
		client:  c,
		ID:      "req",
		dataCh:  make(chan []byte, 4),
		errCh:   make(chan error, 1),
		closeCh: make(chan struct{}),
	}
	c.tunnelStreams["req"] = requester

	// 一个未必可用的入站隧道连接(net.Pipe,未对接),验证被拆除关闭。
	ioR, ioW := net.Pipe()
	tc := newTunnelConn(c, "exe", ioW)
	c.tunnelConns["exe"] = tc
	defer ioR.Close()

	c.closeAllTunnels("transport lost")

	if _, err := requester.Recv(); err == nil {
		t.Error("expected requester stream to be aborted after closeAllTunnels")
	}
	if _, ok := c.tunnelStreams["req"]; ok {
		t.Error("requester stream not removed by closeAllTunnels")
	}
	if _, ok := c.tunnelConns["exe"]; ok {
		t.Error("executor conn not removed by closeAllTunnels")
	}
}
