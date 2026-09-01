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

// kill 终止进程。Stop 不强等:SIGTERM 对 relay 的 signal.Notify 生效,足以优雅退出。
func kill(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
}
