//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detach 让子进程成为独立的会话首领,脱离控制终端,父退出后继续存活。
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// alive 通过信号 0 探测进程是否存在。
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// kill 发送 SIGTERM;relay 的 signal.Notify 会走优雅退出。
func kill(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

// forceKill 是 SIGTERM 超时后的兜底。
func forceKill(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
