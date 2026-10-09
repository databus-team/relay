//go:build windows

package supervisor

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

func prepareCmdAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	prepareCmdAttr(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), ",\""+strconv.Itoa(pid)+"\",")
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	// Windows 上通过 taskkill /F /T 强杀整棵进程树，防止 sh.exe 的子进程孤儿残留
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	prepareCmdAttr(cmd)
	return cmd.Run()
}
