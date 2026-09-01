// Package daemon 提供进程级 daemon 管理:PID 文件 + detached 启动 + 存活探测 + 停止。
// 与系统服务管理器(launchd/systemd/SCM)解耦,便于在 code-server web 终端这类
// 前台环境里把 `relay server` / `relay watch` 拉成一个后台守护进程。
package daemon

import (
	"fmt"
	"io"
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

// replaceBinary 原子地把 src(新二进制)替换到 dst(通常为 os.Executable() 路径):
// 先写入 dst 同目录临时文件再 rename,避免写一半被读。
func replaceBinary(dst, src string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat new binary: %w", err)
	}
	if srcInfo.IsDir() || srcInfo.Size() == 0 {
		return fmt.Errorf("new binary is empty or a directory")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".relay-replace-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	in, err := os.Open(src)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("open source: %w", err)
	}
	_, copyErr := io.Copy(tmp, in)
	in.Close()
	tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("copy: %w", copyErr)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}

// ReplaceBinary 把新二进制原子替换到目标路径(通常为 os.Executable())。供 `upgrade` 使用。
func ReplaceBinary(dst, src string) error { return replaceBinary(dst, src) }

// BackupBinary 把 dst 现行二进制原子复制为 `<dst>.prev`,作为换装前的人工回退备件。
// 现有 replaceBinary 只做 temp+rename、不产生旧文件备份,故 `.prev` 由本函数显式产生。
func BackupBinary(dst string) error {
	info, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat current binary: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("current binary is empty or a directory")
	}

	prev := dst + ".prev"
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".relay-prev-*")
	if err != nil {
		return fmt.Errorf("create prev temp: %w", err)
	}
	tmpName := tmp.Name()
	in, err := os.Open(dst)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("open current binary: %w", err)
	}
	_, copyErr := io.Copy(tmp, in)
	in.Close()
	tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("copy prev: %w", copyErr)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod prev: %w", err)
	}
	if err := os.Rename(tmpName, prev); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("install prev: %w", err)
	}
	return nil
}

// ReplaceBinaryWithKeep 换装:先把现行 dst 备份为 `<dst>.prev`,再原子替换为新 src。
// 备份在前:即使后续替换失败,`.prev` 也已保留,供人工回退(无自动回滚)。供中转/uLeU3 换装使用。
func ReplaceBinaryWithKeep(dst, src string) error {
	if err := BackupBinary(dst); err != nil {
		return err
	}
	return replaceBinary(dst, src)
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
