package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// holderSockProbeTimeout 只用于判断「是否已有 node 在服务这个路径」,不走数据面。
const holderSockProbeTimeout = 300 * time.Millisecond

// listenHolderSocket 为 node 常驻 holder 绑定控制 socket。若同一路径已能被拨通,说明
// 已有 node 在服务它,直接报错而不是摘掉重绑:两个 node 同时存在时,后起的会抢走控制面,
// 数据面(TCP 监听)却留在先起进程里,CLI 于是看到「全部 stopped」且永远 bind 不上。
// 文件存在但拨不通(上一位进程已死)才是陈旧残留,可以安全删除。
func listenHolderSocket(sockPath string) (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", sockPath, holderSockProbeTimeout); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another node is already serving %s", sockPath)
	}
	if dir := filepath.Dir(sockPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(sockPath)
	return net.Listen("unix", sockPath)
}
