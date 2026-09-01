//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	detachedProcess       = 0x00000008 // DETACHED_PROCESS
	createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
)

// detach 以前台无窗口 + 脱离父进程组的方式启动,避免打开新控制台窗口。
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

// alive 用 tasklist 判断进程是否存活(windows 的 Signal(0) 不受支持)。
func alive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	// CSV 形如 "relay.exe","1234","Console",...;只要 PID 出现在第二列即存活。
	return strings.Contains(string(out), ",\""+strconv.Itoa(pid)+"\",")
}

// kill 用 taskkill 强杀。
func kill(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F").Run()
}
