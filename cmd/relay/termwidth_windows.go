//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// termWidth 取控制台窗口的列数;stdout 不是控制台(管道/重定向)或探测失败时返回 0,
// 由调用方按「宽度未知」处理。
func termWidth(f *os.File) int {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(f.Fd()), &info); err != nil {
		return 0
	}
	return int(info.Window.Right-info.Window.Left) + 1
}
