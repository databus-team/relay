//go:build !windows

package supervisor

import (
	"os/exec"
	"syscall"
	"time"
)

func prepareCmdAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	// 尝试对整个进程组发送 SIGTERM
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	// 等待最多 1.5 秒，若未退出则强杀 SIGKILL
	done := make(chan bool, 1)
	go func() {
		for i := 0; i < 15; i++ {
			if !isProcessAlive(pid) {
				done <- true
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		done <- false
	}()

	if !<-done {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		// 单独确保主 PID 受到信号
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}
