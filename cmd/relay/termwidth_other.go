//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris && !aix && !windows

package main

import "os"

// termWidth 在无法探测终端宽度的平台上恒为 0,表格随之按自然宽渲染(不省略)。
func termWidth(*os.File) int { return 0 }
