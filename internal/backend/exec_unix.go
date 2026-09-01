//go:build !windows

package backend

import "os/exec"

// HideConsoleWindow 仅 Windows 有意义(避免子进程弹出 cmd 窗口);其它平台无操作。
func HideConsoleWindow(c *exec.Cmd) {}