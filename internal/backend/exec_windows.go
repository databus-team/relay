//go:build windows

package backend

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW: 终端 app 不新建控制台窗口

// HideConsoleWindow 让派生的 sh -c 子进程不弹出一个新的 cmd 控制台窗口。
// relay watch 通常以后台 daemon 运行(无控制台),直接 spawn console 子进程时
// Windows 会为它新建窗口造成每次 exec 闪烁;加这两个标志后静默执行,输出仍走管道。
func HideConsoleWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}