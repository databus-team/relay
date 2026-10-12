//go:build !windows

package supervisor

import (
	"os/exec"
	"syscall"
	"time"
)

// processTree 在 unix 上不需要额外机制:子进程启动时自成进程组(Setpgid),
// killProcessTree 对整个进程组发信号即可覆盖全部后代。
type processTree struct{}

func (p *processTree) attach(pid int) error { return nil }

func (p *processTree) kill(pid int) error { return killProcessTree(pid) }

func (p *processTree) release() {}

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
