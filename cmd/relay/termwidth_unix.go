//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// termWidth 用 TIOCGWINSZ 取终端的实时列数;fd 不是终端(管道/重定向)或探测失败时返回 0,
// 由调用方按「宽度未知」处理。
func termWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
