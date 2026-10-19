//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx 锁整个文件;不带 LOCKFILE_FAIL_IMMEDIATELY 时阻塞至锁可用。
func lockExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, ^uint32(0), ^uint32(0), ol)
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), ol)
}
