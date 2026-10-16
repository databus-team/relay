package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/user/relay/internal/config"
)

// startForward 是独立转发创建的共享动作:确保本机 node(holder)在线,再下发 open 控制消息。
// 唯一入口是 `relay run -t forward`,控制消息代码不外分叉。warnw 接收非 loopback 与持久化告警
// (生产即调用方 stderr;测试可注入缓冲以断言文案)。
func startForward(cfg *config.Config, executor string, spec *PortForwardSpec, policy string, warnw io.Writer) error {
	if cfg.Backend.Type != "relay" {
		return fmt.Errorf("port-forward requires backend.type=relay, got %q", cfg.Backend.Type)
	}
	if err := ensureLocalRelayNode(cfg, forwardSockPath()); err != nil {
		return err
	}
	resp, err := sendForwardControl(forwardSockPath(), fwdControlMsg{
		Op:       opOpen,
		Port:     spec.LocalPort,
		Spec:     spec,
		Executor: executor,
		Restart:  policy,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("start forward %d: %s", spec.LocalPort, resp.Error)
	}
	// 非 loopback 绑定 = 一个无认证的本地入口。holder 侧也会记一条到 node.log,但那是 node 的
	// stderr(daemon 已重定向),发起者本人看不到,故 CLI 同样打到 warnw(与隧道创建同一姿态)。
	addr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
	if !isLoopbackAddr(spec.BindHost) {
		fmt.Fprintf(warnw, "WARNING: listening on %q (non-loopback). This port has no auth.\n", addr)
	}
	if resp.Warning != "" {
		fmt.Fprintf(warnw, "Warning: %s\n", resp.Warning)
	}
	fmt.Printf("forward %d started: %s -> %s:%d via %s (restart=%s)\n", spec.LocalPort, addr, spec.RemoteHost, spec.RemotePort, executor, policy)
	return nil
}

// stopLocalForward 是本机独立转发「停」的唯一实现:node 在运行时经 holder 下发 close(关闭监听、
// 移除意图);node 未运行时降级为直接移除意图文件里的条目(否则 `--restart=always` 会在下次 node
// 启动时被 restore 重放,与「关闭并遗忘」(docker rm)语义相悖)。停不掉时第二个返回值带上底层原因。
// statePath/sockPath/nodeRunning 由调用方注入,便于在不碰真实 ~/.relay 的前提下覆盖 node 未运行分支。
func stopLocalForward(statePath, sockPath string, port int, nodeRunning bool) (tunnelStopOutcome, error) {
	if port <= 0 {
		return tunnelStopUnknown, nil
	}
	if !nodeRunning {
		// node 未运行:没有 holder 可下发 close,但持久化意图仍须移除。
		removed, rerr := removePersistedForward(statePath, port)
		switch {
		case rerr != nil:
			fmt.Fprintf(os.Stderr, "Warning: remove persisted forward %d: %v\n", port, rerr)
			return tunnelStopUnknown, rerr
		case removed:
			fmt.Printf("node not running; removed persisted forward %d\n", port)
			return tunnelStopRemoved, nil
		default:
			fmt.Printf("node not running; no forward for %d to stop\n", port)
			return tunnelStopAbsent, nil
		}
	}
	resp, err := sendForwardControl(sockPath, fwdControlMsg{Op: opClose, Port: port})
	switch {
	case err != nil:
		// 传输层失败(控制 socket 不可达等):底层原因随 err 交回调用方,不塌缩成「名字不在」。
		fmt.Fprintf(os.Stderr, "Warning: stop forward %d: %v\n", port, err)
		return tunnelStopUnknown, err
	case !resp.OK:
		fmt.Fprintf(os.Stderr, "Warning: stop forward %d: %s\n", port, resp.Error)
		return tunnelStopUnknown, errors.New(resp.Error)
	default:
		if resp.Warning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", resp.Warning)
		}
		fmt.Printf("forward %d stopped\n", port)
		return tunnelStopClosed, nil
	}
}
