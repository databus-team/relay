//go:build !windows

package backend

import "os"

// relayExecShell 返回 `sh -c` 所用 shell 及其进程环境。
// POSIX 平台用系统 sh,环境直接透传。
func relayExecShell() (string, []string) {
	return "sh", os.Environ()
}
