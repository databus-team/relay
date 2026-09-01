//go:build windows

package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// msysBinDirs 常见 Windows POSIX 工具链(Git for Windows / MSYS2)的 bin 目录,按优先级。
var msysBinDirs = []string{
	`C:\Program Files\Git\bin`,
	`C:\Program Files\Git\usr\bin`,
	`C:\Program Files (x86)\Git\bin`,
	`C:\msys64\usr\bin`,
	`C:\git\bin`,
}

// relayExecShell 返回 `sh -c` 所用 shell 路径及进程环境。
// Windows 上 daemon 若由非 msys(如 cmd/任务计划)启动,其 PATH 常缺 Git/msys 的
// usr/bin,导致 shell 与 uname/ls/sleep 等工具解析不到。这里显式解析一个可用的 sh,
// 并把 msys 的 bin 目录补到 PATH 前部,使 relay exec 不与启动环境绑定、重启后自愈。
func relayExecShell() (string, []string) {
	shell := findPOSIXSh()
	env := enhancePath(os.Environ())
	return shell, env
}

// findPOSIXSh 优先取 PATH 里的 sh,其次常见 msys/Git 安装路径;找不到则保底返回 "sh"
// (让上层 exec 报错清晰,而不是 panic)。
func findPOSIXSh() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	for _, d := range msysBinDirs {
		cand := filepath.Join(d, "sh.exe")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	// 也可能装在了别的盘/目录:退而求其次直接在 Git 顶级的 bin 里找。
	if p, err := exec.LookPath("bash"); err == nil {
		return p // bash -c 也兼容 POSIX 脚本
	}
	return "sh"
}

// enhancePath 把找到的 msys bin 目录(及常见的兄弟 usr/bin)插入 PATH 前部。
func enhancePath(env []string) []string {
	dir := shBinDir()
	if dir == "" {
		return env
	}

	var extra []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" {
			return
		}
		if s, err := filepath.Abs(d); err == nil {
			d = s
		}
		if !seen[d] {
			seen[d] = true
			extra = append(extra, d)
		}
	}
	add(dir)
	if up := filepath.Dir(dir); up != dir {
		add(filepath.Join(up, "bin"))       // ...\usr\bin 的同层二进制 -> ...\usr\bin 已在 dir;兜底
		add(filepath.Join(up, "..", "bin")) // ...\usr\bin 上两级的 bin(如 ...\git\bin)
		add(filepath.Join(up, "..", "usr", "bin"))
	}
	if len(extra) == 0 {
		return env
	}

	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+strings.Join(extra, ";")+";"+strings.TrimPrefix(kv, "PATH="))
		} else {
			out = append(out, kv)
		}
	}
	return out
}

// shBinDir 返回一个确认存在 sh.exe 的 bin 目录;找不到返回空串。
func shBinDir() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return filepath.Dir(p)
	}
	for _, d := range msysBinDirs {
		if _, err := os.Stat(filepath.Join(d, "sh.exe")); err == nil {
			return d
		}
	}
	return ""
}
