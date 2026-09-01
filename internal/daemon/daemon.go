// Package daemon 提供进程级 daemon 管理:PID 文件 + detached 启动 + 存活探测 + 停止。
// 与系统服务管理器(launchd/systemd/SCM)解耦,便于在 code-server web 终端这类
// 前台环境里把 `relay server` / `relay watch` 拉成一个后台守护进程。
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func baseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".relay")
}

// PidFile 返回 <name>.pid 的路径(缺省在 ~/.relay 下)。
func PidFile(name string) string { return filepath.Join(baseDir(), name+".pid") }

// LogFile 返回 <name>.log 的路径。
func LogFile(name string) string { return filepath.Join(baseDir(), name+".log") }

// Status 读取 pid 文件并探测进程是否存活。
type StatusInfo struct {
	Running bool
	Pid     int
	Err     error // 非 pid 文件不存在时的读取错误
}

func Get(pidFile string) StatusInfo {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusInfo{}
		}
		return StatusInfo{Err: err}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		// pid 文件损坏,视为未运行
		return StatusInfo{}
	}
	return StatusInfo{Running: alive(pid), Pid: pid}
}

// Start 以当前可执行文件、给定参数(args 含子命令,如 ["server","run"])启动一个
// detached 子进程,stdout/sderr 重定向到 logFile,pid 写入 pidFile。返回子进程 pid。
func Start(selfArgs []string, logFile, pidFile string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0755); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(exe, selfArgs...)
	detach(cmd)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// Stop 读取 pid 文件,若存活则终止并移除 pid 文件;未运行返回错误。
func Stop(pidFile string) error {
	st := Get(pidFile)
	if st.Err != nil {
		return st.Err
	}
	defer os.Remove(pidFile)
	if !st.Running {
		return fmt.Errorf("not running")
	}
	kill(st.Pid)
	return nil
}
