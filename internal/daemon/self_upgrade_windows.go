//go:build windows

package daemon

import "errors"

// SelfUpgrade 在 Windows 上不直接支持覆盖运行中的二进制;远端执行方换装走
// 部署脚本的 detached 换装(RESTART),中转也用 Linux。这里返回不支持。
func SelfUpgrade(newBin string, args []string, logFile string) (int, error) {
	return 0, errors.New("daemon self-upgrade is not supported on windows; use the deploy swap flow")
}
