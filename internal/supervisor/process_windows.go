//go:build windows

package supervisor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	detachedProcess       = 0x00000008 // DETACHED_PROCESS
	createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP

	processSetQuota  = 0x0100
	processTerminate = 0x0001
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

// processTree 用一个 Job Object 收拢任务的全部后代。
//
// 不能只靠 taskkill /T:它按 Windows 记录的父子关系遍历,而 MSYS 的 fork 让这层关系对不上,
// `sh -c 'yes > /dev/null'` 的子进程能在 stop 之后继续跑(实测,任务已记为 stopped/exit 0)。
// Job 的成员身份由内核维护,与父子关系无关,TerminateJobObject 一次带走整棵树。
type processTree struct {
	mu  sync.Mutex
	job uintptr // 0 = 未建立(如启动时认领的遗留进程),只能退回 taskkill
}

// attach 把刚启动的子进程纳入 job。必须在它 fork 出命令之前调用:job 成员身份只被
// "入 job 之后创建"的后代继承,已经存在的子进程追不回来(加载器的启动开销是毫秒级,
// 这一步是微秒级,顺序上够用)。
func (p *processTree) attach(pid int) error {
	job, _, callErr := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return fmt.Errorf("CreateJobObject: %w", callErr)
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)

	if r, _, callErr := procAssignProcessToJobObject.Call(job, uintptr(h)); r == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("AssignProcessToJobObject: %w", callErr)
	}

	p.mu.Lock()
	p.job = job
	p.mu.Unlock()
	return nil
}

// kill 终止整棵树。job 不可用时退回按 pid 的 taskkill,即旧行为(可能漏掉后代)。
func (p *processTree) kill(pid int) error {
	p.mu.Lock()
	job := p.job
	p.mu.Unlock()

	if job == 0 {
		return killProcessTree(pid)
	}
	if r, _, _ := procTerminateJobObject.Call(job, 1); r == 0 {
		p.release()
		return killProcessTree(pid)
	}
	p.release()
	return nil
}

// release 关闭 job 句柄。刻意不设 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:执行方退出后任务本就
// 该活着(下次启动按策略认领或重放),句柄随进程消亡关闭时不该顺带把任务杀了。
func (p *processTree) release() {
	p.mu.Lock()
	job := p.job
	p.job = 0
	p.mu.Unlock()
	if job != 0 {
		syscall.CloseHandle(syscall.Handle(job))
	}
}

func prepareCmdAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	prepareCmdAttr(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), ",\""+strconv.Itoa(pid)+"\",")
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	prepareCmdAttr(cmd)
	return cmd.Run()
}
