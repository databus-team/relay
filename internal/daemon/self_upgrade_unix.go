//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
)

// SelfUpgrade 原子地把新二进制替换到当前可执行文件路径,然后以 detached 方式用相同
// 参数重启(此时磁盘上已是新二进制),返回新进程 pid。调用方在 spawn 成功后应自行退出,
// 这样新进程接管(中转 server 场景同时释放监听端口)。
//
// Linux 上可覆盖运行中的可执行文件:当前进程仍持有旧的 inode,新进程读到新的 inode。
func SelfUpgrade(newBin string, args []string, logFile string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("self upgrade: resolve current exe: %w", err)
	}
	if err := replaceBinary(exe, newBin); err != nil {
		return 0, fmt.Errorf("self upgrade: %w", err)
	}

	// detached 重启,日志到 logFile。
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("self upgrade: open log: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(exe, args...)
	detach(cmd)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("self upgrade: respawn: %w", err)
	}
	return cmd.Process.Pid, nil
}
