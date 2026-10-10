package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/exchange"
	"github.com/user/relay/internal/jobrunner"
	"github.com/user/relay/internal/logx"
	relaybackend "github.com/user/relay/internal/relay/backend"
	"github.com/user/relay/internal/relay/protocol"
	_ "github.com/user/relay/internal/relay/server"
	"github.com/user/relay/internal/sync/git"
	"github.com/user/relay/internal/sysinfo"
	"github.com/user/relay/internal/version"
	"github.com/user/relay/internal/watcher"
)

var (
	app = kingpin.New("relay", "Generic File Exchange Command Execution System")

	_ = kingpin.CommandLine

	configPath = kingpin.Flag("config", "Path to config file").Short('c').Default("~/.relay/config.yaml").String()
	debugFlag  = kingpin.Flag("debug", "Enable debug mode").Bool()

	// Watch command - continuous monitoring (legacy)
	watchCmd = kingpin.Command("watch", "Workspace watcher daemon (deprecated, use node instead)")

	// 位置参数 action:默认前台;run=前台;start/stop/status/restart/upgrade 为 daemon 控制。
	watchAction = watchCmd.Arg("action", "run|start|stop|status|restart|upgrade (default: run)").HintOptions("run", "start", "stop", "status", "restart", "upgrade").String()
	// upgrade 用的新二进制路径(仅 action=upgrade 时使用)。
	watchUpgradePath = watchCmd.Arg("upgrade-path", "Path to new relay binary (with action=upgrade)").String()

	// Pull command - download single file (requires filename)
	pullCmd    = kingpin.Command("pull", "Pull file from remote workspace")
	pullWatch  = pullCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').HintAction(getWorkspaceHints).String()
	pullFile   = pullCmd.Arg("filename", "Remote filename to download").Required().String()
	pullDelete = pullCmd.Flag("delete", "Delete the remote file after a successful pull").Short('d').Bool()

	// List command - list remote directory
	listCmd   = kingpin.Command("list", "List files in remote watch directory")
	listWatch = listCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').HintAction(getWorkspaceHints).String()

	// Push command - upload files, optionally without running workspace jobs
	pushCmd    = kingpin.Command("push", "Push files to remote workspace and trigger jobs")
	pushWatch  = pushCmd.Flag("watch", "Target workspace ID (defaults to current directory name); mutually exclusive with --node/--executor").Short('w').HintAction(getWorkspaceHints).String()
	pushExec   = pushCmd.Flag("executor", "Route directly to this executor (executor_id) without a workspace binding").Short('e').HintAction(getNodeHints).String()
	pushNode   = pushCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()
	pushSrc    = pushCmd.Arg("source", "Source file to push").Required().String()
	pushDest   = pushCmd.Flag("dest", "Destination absolute path on the executor (defaults to <watch_dir>/<filename>; requires --no-jobs)").String()
	pushNoJobs = pushCmd.Flag("no-jobs", "Transfer only; do not run workspace jobs on the remote").Bool()

	// Job command - run config-defined jobs locally
	jobCmd      = kingpin.Command("job", "Run configured jobs locally")
	jobRun      = jobCmd.Command("run", "Run a configured job manually on local node")
	jobRunWatch = jobRun.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').HintAction(getWorkspaceHints).String()
	jobRunID    = jobRun.Arg("jobid", "Job ID to run").Required().String()
	jobRunFile  = jobRun.Arg("file", "Local file to bind to {file_path} and friends").String()

	// Run command - command forwarding & execution (alias: exec)
	execCmd         = kingpin.Command("run", "Run command on remote node (alias: exec)").Alias("exec")
	execWatch       = execCmd.Flag("watch", "Target workspace ID (defaults to current directory name); mutually exclusive with --node/--executor").Short('w').HintAction(getWorkspaceHints).String()
	execExec        = execCmd.Flag("executor", "Route directly to this executor (executor_id) without a workspace binding").Short('e').HintAction(getNodeHints).String()
	execNode        = execCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()
	execForceRemote = execCmd.Flag("force-remote", "Force overwrite remote state (creates shadow snapshot on remote first)").Short('f').Bool()
	execNoSync      = execCmd.Flag("no-sync", "Skip git incremental sync before execution").Bool()
	execDetach      = execCmd.Flag("detach", "Run command in background as daemon task (returns TaskID)").Short('d').Bool()
	// 支持多词命令:relay run git status 会收成 ["git","status"] 再空格 join。
	// 含以 - 开头参数的命令(如 git log --oneline)仍须整体加引号,避免被当 flag。
	execCmdStr = execCmd.Arg("command", "Command to execute (multiple words are joined)").Required().Strings()

	// ps command - 查询后台任务
	psCmd  = kingpin.Command("ps", "List managed background tasks (similar to docker ps)")
	psAll  = psCmd.Flag("all", "Show all tasks (default shows running only)").Short('a').Bool()
	psExec = psCmd.Flag("executor", "Filter tasks by executor node").Short('e').HintAction(getNodeHints).String()
	psNode = psCmd.Flag("node", "Filter tasks by node ID (alias for --executor)").HintAction(getNodeHints).String()
	psJSON = psCmd.Flag("json", "Output as JSON").Bool()

	// logs command - 查看任务日志
	logsCmd    = kingpin.Command("logs", "Tail or stream background task logs (similar to docker logs)")
	logsTaskID = logsCmd.Arg("task-id", "Task ID to view").Required().String()
	logsFollow = logsCmd.Flag("follow", "Follow log output").Short('f').Bool()
	logsTail   = logsCmd.Flag("tail", "Number of lines to show from the end").Default("100").Int()
	logsExec   = logsCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	logsNode   = logsCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	// stop command - 终止后台任务
	stopCmd    = kingpin.Command("stop", "Terminate running task process tree (similar to docker stop)")
	stopTaskID = stopCmd.Arg("task-id", "Task ID to stop").Required().String()
	stopExec   = stopCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	stopNode   = stopCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	// Nodes command - 查看全网节点拓扑状态与角色 (类似 kubectl get nodes / docker node ls; alias: status)
	nodesCmd      = kingpin.Command("nodes", "List cluster peer nodes and topology (alias: status)").Alias("status")
	nodesJSONOut  = nodesCmd.Flag("json", "Output as JSON").Bool()
	nodesUsageOut = nodesCmd.Flag("usage", "Query dynamic CPU load & memory usage of nodes (heavier probe)").Short('u').Bool()

	// Cleanup command - remove stale command files
	cleanupCmd   = kingpin.Command("cleanup", "Clean up stale remote commands and results")
	cleanupWatch = cleanupCmd.Flag("watch", "Target watch ID").Short('w').Required().HintAction(getWorkspaceHints).String()

	// Sync command - push config to remote watcher for hot reload
	syncCmd      = kingpin.Command("sync", "Push config to remote node for live reload")
	syncExecutor = syncCmd.Flag("executor", "Target executor's executor_id (default = this config's backend executor_id)").Short('e').HintAction(getNodeHints).String()
	syncNode     = syncCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	// Workspaces command - list configured workspaces from config
	// (alias: `workspaces`; kingpin v2 doesn't render aliases in --help)
	wsCmd     = kingpin.Command("ws", "List configured workspaces (alias: workspaces)").Alias("workspaces")
	wsName    = wsCmd.Flag("name", "Show details for a specific workspace ID").HintAction(getWorkspaceHints).String()
	wsJSON    = wsCmd.Flag("json", "Output as JSON").Bool()
	wsVerbose = wsCmd.Flag("verbose", "Show detailed table output").Short('v').Bool()

	// Version command - 版本查看(纯本地构建信息)。远端台账职责已归一到 `status`。
	versionCmd     = kingpin.Command("version", "Show build version information")
	versionJSONOut = versionCmd.Flag("json", "Output as JSON").Bool()

	// server-remote command - 一键部署中转(受控自升级):上传新二进制 → 中转自检 → 换装 → 核验。
	serverRemoteCmd = kingpin.Command("server-remote", "Deploy and self-upgrade transit server remotely")
	serverRemoteBin = serverRemoteCmd.Flag("binary", "Path to the new relay binary to send (default: current executable)").String()
	serverRemoteExpect = serverRemoteCmd.Flag("expect", "Expected transit version after swap (default: this relay's version.String())").String()

	tunnelCmd      = kingpin.Command("tunnel", "Open SOCKS5 egress tunnel through remote node")
	tunnelListen   = tunnelCmd.Flag("listen", "Local SOCKS5 listen address").Default("127.0.0.1:1080").String()
	tunnelExecName = tunnelCmd.Flag("executor", "Egress executor's executor_id (see `relay status` -> executors[].executor_id)").Short('w').HintAction(getNodeHints).String()
	tunnelNodeName = tunnelCmd.Flag("node", "Egress node ID (alias for --executor)").HintAction(getNodeHints).String()
	tunnelJSON     = tunnelCmd.Flag("json", "Output as JSON").Bool()
	tunnelFollow   = tunnelCmd.Flag("follow", "Follow log output (with action=logs)").Short('f').Bool()
	tunnelTail     = tunnelCmd.Flag("tail", "Number of lines to show from the end (with action=logs)").Default("100").Int()
	tunnelAction   = tunnelCmd.Arg("action", "run|start|stop|status|logs|restart|upgrade (default: run)").HintOptions("run", "start", "stop", "status", "logs", "restart", "upgrade").String()
	tunnelTargetArg = tunnelCmd.Arg("target-or-path", "Target node ID (with action=logs) or new binary path (with action=upgrade)").String()

	// Port-forward command - 本地端口映射到远端服务 (类似 kubectl port-forward / docker -p)
	portForwardCmd    = kingpin.Command("port-forward", "Forward local port to remote node (alias: pf)").Alias("pf")
	portForwardPorts  = portForwardCmd.Arg("port", "[local:]remote port (e.g. 8080:8080 or 8080)").String()
	portForwardAction = portForwardCmd.Arg("action", "run|start|stop|status|restart (default: run)").HintOptions("run", "start", "stop", "status", "restart").String()
	portForwardExec   = portForwardCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	portForwardNode   = portForwardCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()
	portForwardWatch  = portForwardCmd.Flag("watch", "Target workspace ID (infers executor)").Short('w').HintAction(getWorkspaceHints).String()
	portForwardBind   = portForwardCmd.Flag("bind", "Local IP to bind to").Default("127.0.0.1").String()

	// Undo command - 一键恢复远端工作区影子快照 (防误操作与冲突覆盖)
	undoCmd      = kingpin.Command("undo", "Restore remote workspace shadow snapshot")
	undoExec     = undoCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	undoNode     = undoCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()
	undoWatch    = undoCmd.Flag("watch", "Target workspace ID (infers executor)").Short('w').HintAction(getWorkspaceHints).String()
	undoList     = undoCmd.Flag("list", "List available snapshots on remote node").Short('l').Bool()
	undoSnapshot = undoCmd.Flag("snapshot", "Specific snapshot ref to restore (default: latest)").String()

	// Diff command - 对比本地与远端 Git 状态及冲突预警
	diffCmd   = kingpin.Command("diff", "Inspect workspace Git drift and conflict warnings")
	diffExec  = diffCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	diffNode  = diffCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()
	diffWatch = diffCmd.Flag("watch", "Target workspace ID (infers executor)").Short('w').HintAction(getWorkspaceHints).String()
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "__complete" || os.Args[1] == "--completion-bash") {
		handleComplete(os.Args[2:])
		return
	}

	kingpin.CommandLine.HelpFlag.Short('h')
	setupUsageTemplate()

	// 统一日志基座:timetamps + 可选 verbose。所有端点(server/watch/exec/...)
	// 的 log/Info 级输出都带日期时间,便于审计;--debug 额外打开 verbose 详情。
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *debugFlag {
		backend.SetDebug(true)
		logx.SetDebug(true)
	}

	switch kingpin.Parse() {
	case serverCmd.FullCommand():
		// action 位置参数:默认/run=前台;start/stop/status/restart=daemon。裸 `relay server`
		// (无 action)即为前台,保留原有用法。
		switch normalizedAction(*serverAction) {
		case "start":
			if err := daemonStart("server"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			daemonStop("server")
		case "status":
			if err := daemonStatus("server"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "restart":
			daemonRestart("server")
		case "upgrade":
			daemonUpgrade("server", *serverUpgradePath)
		default:
			if err := runServer(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}
	case nodeCmd.FullCommand():
		switch normalizedAction(*nodeAction) {
		case "start":
			if err := daemonStart("node"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			daemonStop("node")
		case "status":
			if err := daemonStatus("node"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "restart":
			daemonRestart("node")
		case "upgrade":
			daemonUpgrade("node", *nodeUpgradePath)
		default:
			if err := runNode(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}
	case watchCmd.FullCommand():
		switch normalizedAction(*watchAction) {
		case "start":
			if err := daemonStart("watch"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			daemonStop("watch")
		case "status":
			if err := daemonStatus("watch"); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "restart":
			daemonRestart("watch")
		case "upgrade":
			daemonUpgrade("watch", *watchUpgradePath)
		default:
			runWatch()
		}
	case pullCmd.FullCommand():
		runPull()
	case pushCmd.FullCommand():
		runPush()
	case execCmd.FullCommand():
		runExec()
	case nodesCmd.FullCommand():
		runNodes()
	case listCmd.FullCommand():
		runList()
	case cleanupCmd.FullCommand():
		runCleanup()
	case "help":
		app.Usage(os.Args)
	case syncCmd.FullCommand():
		runSync()
	case wsCmd.FullCommand():
		runWorkspaces()
	case versionCmd.FullCommand():
		runVersion()
	case psCmd.FullCommand():
		runPs()
	case logsCmd.FullCommand():
		runLogs()
	case stopCmd.FullCommand():
		runStop()
	case serverRemoteCmd.FullCommand():
		runServerRemote()
	case completionCmd.FullCommand():
		runCompletion()
	case jobRun.FullCommand():
		runJobRun()
	case tunnelCmd.FullCommand():
		switch normalizedAction(*tunnelAction) {
		case "start":
			if err := daemonStartTunnel(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			daemonStopTunnel()
		case "status":
			if err := daemonStatusTunnel(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "logs":
			if err := daemonLogsTunnel(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "restart":
			daemonRestartTunnel()
		case "upgrade":
			daemonUpgradeTunnel(*tunnelTargetArg)
		default:
			runTunnel()
		}
	case portForwardCmd.FullCommand():
		act := normalizedAction(*portForwardAction)
		if act == "" && (*portForwardPorts == "status" || *portForwardPorts == "list") {
			act = "status"
			*portForwardPorts = ""
		}
		switch act {
		case "start":
			if err := daemonStartPortForward(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			daemonStopPortForward()
		case "status":
			if err := daemonStatusPortForward(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		default:
			runPortForward()
		}
	case undoCmd.FullCommand():
		runUndo()
	case diffCmd.FullCommand():
		runDiff()
	default:
		app.Usage(os.Args)
	}
}

// normalizedAction 把 action 位置参数归一:run 或空视为前台(返回 ""),其余 daemon 动作原样返回。
func normalizedAction(a string) string {
	switch a {
	case "run", "":
		return ""
	case "start", "stop", "status", "logs", "restart", "upgrade":
		return a
	default:
		return ""
	}
}

// daemonStart 已运行则提示,否则 detached 拉起 `relay <name> run -c <config>` 并记录 pid。
func daemonStart(name string) error {
	return daemonStartArgs(name, []string{name, "run", "-c", *configPath})
}

// daemonStartArgs 以 instance 为 pid/log 文件标识,用给定 args(含子命令,如 ["tunnel","run",...])
// detached 启动一个 daemon。多实例(如隧道)与单实例(server/watch)共用此核心,仅文件标识不同。
func daemonStartArgs(instance string, args []string) error {
	pidFile, logFile := daemon.PidFile(instance), daemon.LogFile(instance)
	st := daemon.Get(pidFile)
	if st.Err != nil {
		return st.Err
	}
	if st.Running {
		fmt.Printf("%s already running (pid %d)\n", instance, st.Pid)
		return nil
	}
	pid, err := daemon.Start(args, logFile, pidFile)
	if err != nil {
		return fmt.Errorf("daemon start %s: %w", instance, err)
	}
	fmt.Printf("%s started (pid %d)\nlog: %s\n", instance, pid, logFile)
	return nil
}

// daemonStop 停止;未运行视为幂等成功。
func daemonStop(name string) {
	if err := daemon.Stop(daemon.PidFile(name)); err != nil {
		fmt.Printf("%s is not running\n", name)
		return
	}
	fmt.Printf("%s stopped\n", name)
}

// daemonRestart 先停再启。
func daemonRestart(name string) {
	daemonRestartArgs(name, []string{name, "run", "-c", *configPath})
}

// daemonRestartArgs 与 daemonRestart 相同,但按实例标识 + 自定义 args 重启(供多实例隧道复用)。
func daemonRestartArgs(instance string, args []string) {
	pidFile, logFile := daemon.PidFile(instance), daemon.LogFile(instance)
	st := daemon.Get(pidFile)
	if st.Err == nil && st.Running {
		_ = daemon.Stop(pidFile)
	}
	pid, err := daemon.Start(args, logFile, pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Printf("%s restarted (pid %d)\nlog: %s\n", instance, pid, logFile)
}

// daemonUpgrade 按需升级:停 daemon → 原子替换自身二进制 → 用新二进制重启。
// 这是中转/执行方"self-update + reboot"的入口,仅在被调用时动作(非常驻)。
func daemonUpgrade(name, newBin string) {
	daemonUpgradeArgs(name, newBin, []string{name, "run", "-c", *configPath})
}

// daemonUpgradeArgs 按实例标识 + 自定义 args 升级重启(供多实例隧道复用)。
func daemonUpgradeArgs(instance, newBin string, args []string) {
	if newBin == "" {
		fmt.Fprintf(os.Stderr, "Error: action 'upgrade' requires a binary path argument\n")
		return
	}
	pidFile, logFile := daemon.PidFile(instance), daemon.LogFile(instance)
	_ = daemon.Stop(pidFile) // 若在跑先停(释放占用,尤其 Windows)

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: resolve exe: %v\n", err)
		return
	}
	if err := daemon.ReplaceBinary(exe, newBin); err != nil {
		fmt.Fprintf(os.Stderr, "Error: replace binary: %v\n", err)
		return
	}
	pid, err := daemon.Start(args, logFile, pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: restart %s: %v\n", instance, err)
		return
	}
	fmt.Printf("%s upgraded & restarted (pid %d, binary=%s)\nlog: %s\n", instance, pid, newBin, logFile)
}

// daemonStatus 报告 pid 状态与日志路径。
func daemonStatus(name string) error {
	pidFile, logFile := daemon.PidFile(name), daemon.LogFile(name)
	st := daemon.Get(pidFile)
	if st.Err != nil {
		return st.Err
	}
	if st.Running {
		fmt.Printf("%s: running (pid %d)\nlog: %s\n", name, st.Pid, logFile)
	} else {
		fmt.Printf("%s: stopped\nlog: %s\n", name, logFile)
	}
	return nil
}

// tunnelInstanceName 由 executor 派生隧道 daemon 的实例名(pid/log 文件名用,形如
// `tunnel-<executor>`)。executor 是执行方注册的身份,可能含非法文件名字符,做净化后作后缀,
// 保证每条隧道一个独立 pid/log 文件、可多开;status 按 `ListInstances("tunnel")` 枚举它们。
func tunnelInstanceName(executor string) string {
	var b strings.Builder
	b.WriteString("tunnel-")
	for _, r := range executor {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// tunnelDaemonArgs 构造隧道 daemon 子进程的启动参数:`tunnel run` 后透传 -w/--listen/-c,
// 使子进程与前台 `relay tunnel -w <executor> --listen <addr>` 行为一致。
// 注意 --listen 未定义 `-l` 短旗标,须用长旗标全名。
func tunnelDaemonArgs() []string {
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	return []string{"tunnel", "run", "-w", target, "--listen", *tunnelListen, "-c", *configPath}
}

// tunnelRequireExecutor 校验隧道控制类操作(启动/停止/重启/升级)必须携带 node/executor。
func tunnelRequireExecutor() error {
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	if target == "" {
		return fmt.Errorf("Specify --node or --executor with the egress node ID (see `relay status` -> `executors[].executor_id`)")
	}
	return nil
}

// daemonStartTunnel 以 executor 命名的实例 detached 启动隧道 daemon。
func daemonStartTunnel() error {
	if err := tunnelRequireExecutor(); err != nil {
		return err
	}
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	return daemonStartArgs(tunnelInstanceName(target), tunnelDaemonArgs())
}

// daemonStopTunnel 停止指定 executor 的隧道实例。
func daemonStopTunnel() {
	if err := tunnelRequireExecutor(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	daemonStop(tunnelInstanceName(target))
}

// daemonRestartTunnel 重启指定 executor 的隧道实例。
func daemonRestartTunnel() {
	if err := tunnelRequireExecutor(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	daemonRestartArgs(tunnelInstanceName(target), tunnelDaemonArgs())
}

// daemonUpgradeTunnel 升级重启指定 executor 的隧道实例。
func daemonUpgradeTunnel(newBin string) {
	if err := tunnelRequireExecutor(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	daemonUpgradeArgs(tunnelInstanceName(target), newBin, tunnelDaemonArgs())
}

func runWatch() {
	// Resolve a leading ~ so the path stored on the watcher (used later for
	// config backup during sync) is an absolute filesystem path, not a literal
	// "~" that os.OpenFile can't read.
	cfgPath, err := config.ExpandHome(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to expand config path: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	// 本进程是执行方(watch 端):显式置位 executor 角色。
	// 这样配置里的 executor: true 只在 relay watch 生效;本地 CLI(exec/push/sync)
	// 即便共用同一份包含 executor 的配置,也不会自注册为执行方、把命令转发回本机。
	relaybackend.SetExecutorRole(true)

	w, err := watcher.New(cfg, cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create watcher: %v\n", err)
		os.Exit(1)
	}

	// 执行方(relay executor)要把「push 落地后本地跑 jobs」接到自身的 watch 配置与 job 执行逻辑。
	w.SetPushJobHandler(runLocalJobsForPush)

	if err := w.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}
}

func runPull() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchCfg, err := lookupWatch(cfg, *pullWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	remotePath := watchCfg.WatchDir + "/" + *pullFile

	data, err := b.Read(ctx, remotePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read remote file: %v\n", err)
		os.Exit(1)
	}

	// Write to current directory with same filename
	localPath := *pullFile
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write local file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pulled: %s -> %s\n", remotePath, localPath)

	if *pullDelete {
		if err := b.Delete(ctx, remotePath); err != nil {
			// Deleting the remote file is a cleanup step, not part of the pull
			// itself. Report it but keep the successful pull as the outcome.
			fmt.Fprintf(os.Stderr, "WARNING: failed to delete remote file %s: %v\n", remotePath, err)
		} else {
			fmt.Printf("Deleted remote: %s\n", remotePath)
		}
	}
}

func runList() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchCfg, err := lookupWatch(cfg, *listWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	files, err := b.ListDir(ctx, watchCfg.WatchDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "List error: %v\n", err)
		os.Exit(1)
	}

	if len(files) == 0 {
		fmt.Println("(empty directory)")
		return
	}

	fmt.Printf("%-40s %10s  %s\n", "NAME", "SIZE", "MODIFIED")
	fmt.Println(strings.Repeat("-", 65))
	for _, f := range files {
		dirMarker := "-"
		if f.IsDir {
			dirMarker = "d"
		}
		fmt.Printf("%-40s %10s  %s [%s]\n", f.Name, formatSize(f.Size), f.ModTime, dirMarker)
	}
}

func runWorkspaces() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watches, err := resolveWatches(cfg, *wsName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to find workspace: %v\n", err)
		os.Exit(1)
	}

	if *wsJSON {
		out := make([]workspaceJSON, len(watches))
		for i, w := range watches {
			out[i] = toWorkspaceJSON(w)
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to marshal workspaces: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	if *wsVerbose {
		printWorkspacesTable(watches)
		return
	}

	for _, w := range watches {
		fmt.Println(w.ID)
	}
}

// resolveWatches returns the slice of workspaces to render. When name is
// non-empty it looks up the single matching workspace; when name is empty it
// returns the full list. Errors propagate from config.GetWorkspaceByID (e.g. "watch
// not found: <id>") so callers can surface them with their own exit handling.
func resolveWatches(cfg *config.Config, name string) ([]config.WorkspaceConfig, error) {
	if name == "" {
		return cfg.Workspaces, nil
	}
	w, err := cfg.GetWorkspaceByID(name)
	if err != nil {
		return nil, err
	}
	return []config.WorkspaceConfig{*w}, nil
}

// resolveNodeTarget 统一处理 --node 与 --executor 旗标，优先取非空值
func resolveNodeTarget(nodeVal, execVal string) string {
	if nodeVal != "" {
		return nodeVal
	}
	return execVal
}

// getBackendWithAdaptiveLoopback 根据配置构造后端，若当前节点同时运行 Hub，则自适应走本地回环 URL
func getBackendWithAdaptiveLoopback(cfg *config.Config) (backend.FileTransferBackend, error) {
	backendConfig := make(map[string]interface{})
	for k, v := range cfg.Backend.Config {
		backendConfig[k] = v
	}

	// 自适应回环: 若本节点也是 Hub (配置了 server 且可能处于本地监听)，且当前 backend 是 relay
	if cfg.IsHub() && cfg.Backend.Type == "relay" {
		localURL := cfg.GetLocalHubURL()
		if localURL != "" {
			backendConfig["url"] = localURL
		}
	}

	return backend.NewBackend(cfg.Backend.Type, backendConfig)
}

// resolveWorkspaceID returns the watch ID a command should target. An explicit
// -w value wins; otherwise the current working directory's basename is matched
// against the configured watch ids. When the basename matches zero or more than
// one workspace, it fails with the list of available workspaces so the caller
// can prompt the user instead of guessing.
func resolveWorkspaceID(cfg *config.Config, provided string) (string, error) {
	if provided != "" {
		return provided, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %w", err)
	}
	base := filepath.Base(cwd)

	var matches []string
	for _, w := range cfg.Workspaces {
		if w.ID == base {
			matches = append(matches, w.ID)
		}
	}

	if len(matches) > 1 {
		return "", fmt.Errorf("current directory %q matches multiple workspaces (%s); specify -w. Available: %s", base, strings.Join(matches, ", "), joinAvailable(cfg.Workspaces))
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no workspace matches current directory %q; specify -w. Available: %s", base, joinAvailable(cfg.Workspaces))
	}
	return matches[0], nil
}

// joinAvailable renders the configured workspaces as "id (local_dir)" for
// error messages. It is only invoked on the error paths.
func joinAvailable(watches []config.WorkspaceConfig) string {
	parts := make([]string, 0, len(watches))
	for _, w := range watches {
		parts = append(parts, w.ID+" ("+w.LocalDir+")")
	}
	return strings.Join(parts, ", ")
}

// lookupWatch resolves an optional -w value (or the cwd-inferred workspace) to
// its WorkspaceConfig. It separates workspace resolution from the commands that
// just need the config.
func lookupWatch(cfg *config.Config, provided string) (*config.WorkspaceConfig, error) {
	watchID, err := resolveWorkspaceID(cfg, provided)
	if err != nil {
		return nil, err
	}
	return cfg.GetWorkspaceByID(watchID)
}

// workspaceJSON is the on-the-wire shape for `relay ws --json`. It mirrors
// config.WorkspaceConfig but uses lower-case JSON keys so consumers can pipe into
// jq / scripts without depending on Go's default field capitalization.
type workspaceJSON struct {
	ID       string          `json:"id"`
	WatchDir string          `json:"watch_dir"`
	LocalDir string          `json:"local_dir"`
	Paths    []string        `json:"paths"`
	Jobs     []jobConfigJSON `json:"jobs"`
	Executor string          `json:"executor,omitempty"`
}

type jobConfigJSON struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Cmd     string `json:"cmd,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	If      string `json:"if,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

func toWorkspaceJSON(w config.WorkspaceConfig) workspaceJSON {
	jobs := make([]jobConfigJSON, len(w.Jobs))
	for i, j := range w.Jobs {
		jobs[i] = jobConfigJSON{
			ID:      j.ID,
			Type:    j.Type,
			Cmd:     j.Cmd,
			Cwd:     j.Cwd,
			If:      j.If,
			Timeout: j.Timeout,
		}
	}
	return workspaceJSON{
		ID:       w.ID,
		WatchDir: w.WatchDir,
		LocalDir: w.LocalDir,
		Paths:    w.Paths,
		Jobs:     jobs,
		Executor: w.Executor,
	}
}

// printWorkspacesTable renders a 4-column summary of configured workspaces
// with column widths sized to the longest cell in each column (subject to a
// min width equal to the column header). A row that exceeds its column width
// is truncated by fmt's %-*s.
func printWorkspacesTable(watches []config.WorkspaceConfig) {
	idW := len("ID")
	remoteW := len("REMOTE_DIR")
	localW := len("LOCAL_DIR")
	jobsW := len("JOBS")
	for _, w := range watches {
		if n := len(w.ID); n > idW {
			idW = n
		}
		if n := len(w.WatchDir); n > remoteW {
			remoteW = n
		}
		if n := len(w.LocalDir); n > localW {
			localW = n
		}
	}

	// Right-aligned numeric column gets a small extra pad so it doesn't sit
	// flush against the previous column's data.
	const jobsFieldExtra = 4

	separator := strings.Repeat("-", idW+1+remoteW+1+localW+1+jobsW+jobsFieldExtra)

	fmt.Printf("%-*s %-*s %-*s %*s\n", idW, "ID", remoteW, "REMOTE_DIR", localW, "LOCAL_DIR", jobsW+jobsFieldExtra, "JOBS")
	fmt.Println(separator)
	for _, w := range watches {
		fmt.Printf("%-*s %-*s %-*s %*d\n",
			idW, w.ID,
			remoteW, w.WatchDir,
			localW, w.LocalDir,
			jobsW+jobsFieldExtra, len(w.Jobs),
		)
	}
}

func runPush() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	targetNode := resolveNodeTarget(*pushNode, *pushExec)
	if targetNode != "" && *pushWatch != "" {
		fmt.Fprintln(os.Stderr, "Error: --node/--executor and --watch are mutually exclusive")
		os.Exit(1)
	}

	// 目标:直接按 node_id/executor_id 寻址(不经 workspace),或经 -w/当前目录解析到绑定的 executor。
	var target string
	var watchDir string
	if targetNode != "" {
		target = targetNode // 节点直连,无 workspace 视角
	} else {
		watchCfg, err := lookupWatch(cfg, *pushWatch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		watchDir = watchCfg.WatchDir
		target = watchCfg.Executor
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	src := *pushSrc

	filename := filepath.Base(src)

	// --dest 只能与 --no-jobs 一起用(true 时路径可越过 watch_dir 到达执行方任何位置)。
	if *pushDest != "" && !*pushNoJobs {
		fmt.Fprintf(os.Stderr, "Error: --dest requires --no-jobs (a push with workspace jobs has a fixed <watch_dir>/<filename> target)\n")
		os.Exit(1)
	}

	info, err := os.Stat(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stat source: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		pushDir(ctx, b, src, watchDir)
		fmt.Println("Push completed successfully")
		return
	}

	// 直接寻址时无 workspace 视角:默认落到执行方自身 exec_dir 下的 basename,
	// 也可经 --dest 覆盖为绝对路径(部署常用)。workspace 路由时落到 <watch_dir>/<file>。
	dest := filename
	if *pushExec == "" {
		dest = watchDir + "/" + filename
	}
	if *pushNoJobs && *pushDest != "" {
		dest = *pushDest
	}

	// content 仅在需要它的后端分支内读取;普通后端走 pushFile 自行读,
	// 避免为不消费内容的路径重复整文件读。
	readContent := func() []byte {
		content, rerr := os.ReadFile(src)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "Failed to read source: %v\n", rerr)
			os.Exit(1)
		}
		return content
	}

	// 纯下发(不跑 workspace job):走 Jobs=false 通道,落到 dest(绝对)或目标 executor 目录。
	// 路由到 -w 绑定的 executor,或 --executor 直接指定的 executor;空则单根回退。
	targetExecutor := target
	if *pushNoJobs {
		if tn, ok := b.(backend.PushNoJobsSender); ok {
			exit, perr := tn.PushNoJobs(ctx, targetExecutor, dest, readContent())
			if perr != nil {
				fmt.Fprintf(os.Stderr, "Push error: %v\n", perr)
				os.Exit(1)
			}
			if exit != 0 {
				os.Exit(exit)
			}
			fmt.Println("Push completed successfully")
			return
		}
		fmt.Fprintf(os.Stderr, "Error: backend %q has no transport-only push\n", cfg.Backend.Type)
		os.Exit(1)
	}

	// 直达后端(relay):把文件直接送到目标 executor 并触发其本地 jobs;输出流式显示。
	if pj, ok := b.(backend.PushJobSender); ok {
		exit, perr := pj.PushJob(ctx, targetExecutor, dest, readContent(), func(c backend.ExecChunk) {
			if c.Stdout {
				os.Stdout.WriteString(c.Data)
			} else {
				os.Stderr.WriteString(c.Data)
			}
		})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "Push error: %v\n", perr)
			os.Exit(1)
		}
		if exit != 0 {
			os.Exit(exit)
		}
		fmt.Println("Push completed successfully")
		return
	}

	pushFile(ctx, b, src, dest)
	fmt.Println("Push completed successfully")
}

// runLocalJobsForPush 执行方收到直达 push 落地文件后,在本地按工作区跑 jobs,
// 断言 job 输出流回请求方,返回 0 全成功、非 0 有失败。
func runLocalJobsForPush(watchID, absPath string, out func(backend.ExecChunk)) int {
	cfg, err := config.Load(*configPath)
	if err != nil {
		out(backend.ExecChunk{Stdout: false, Data: "push-job: load config: " + err.Error() + "\n"})
		return 1
	}
	watchCfg := resolvePushWorkspace(cfg, watchID, absPath)
	if watchCfg == nil {
		out(backend.ExecChunk{Stdout: false, Data: pushWorkspaceErrHints(cfg, watchID, absPath)})
		return 1
	}

	ctx := context.Background()
	step := 0
	exit := jobrunner.RunJobs(ctx, watchCfg, absPath, func(st jobrunner.Step) {
		r := st.Result
		if st.Running {
			step++
			out(backend.ExecChunk{Stdout: true, Data: fmt.Sprintf("[jobs %d/%d] ▶ %s (%s)\n", step, st.Total, r.JobID, r.Type)})
			return
		}
		mark, okc := "✔", "ok"
		if r.Err != nil {
			mark, okc = "✘", "failed"
		}
		dur := r.Duration.Round(time.Millisecond)
		out(backend.ExecChunk{Stdout: r.Err == nil, Data: fmt.Sprintf("[jobs %d/%d] %s %s %s (exit=%d, %s)\n", step, st.Total, mark, r.JobID, okc, r.ExitCode, dur)})
		// 失败时把该 job 捕获到的输出(stdout/stderr)一并回显,缓冲即为此用,便于定位根因
		if r.Err != nil {
			echo := func(stdout bool, text string) {
				for _, line := range strings.Split(text, "\n") {
					if strings.TrimSpace(line) == "" {
						continue
					}
					out(backend.ExecChunk{Stdout: stdout, Data: "      │ " + line + "\n"})
				}
			}
			echo(true, r.Stdout)
			echo(false, r.Stderr)
		}
	})
	return exit
}

// resolvePushWorkspace 决定 push 落地后该跑哪个 workspace 的 jobs。
// 优先级:
//  1. sess.ExecutorID 恰是一个 workspace id → 精确命中。
//  2. 执行方(以 executorID 标识)显式绑定的 workspaces(Executor == executorID)→ 若唯一直接返回,
//     多个则由落盘路径 absPath 其中做路径消歧。
//  3. 回退:在全部 workspaces 中用 absPath(形如 <executor_root>/<workspace>/<file>,
//     匹配 watch_dir 字符串或 id 路径段)推导。
func resolvePushWorkspace(cfg *config.Config, executorID, absPath string) *config.WorkspaceConfig {
	if wc, err := cfg.GetWorkspaceByID(executorID); err == nil {
		return wc
	}
	if owned := cfg.GetWorkspacesByExecutor(executorID); len(owned) > 0 {
		if len(owned) == 1 {
			return owned[0]
		}
		if wc := disambiguateWorkspacesByPath(owned, absPath); wc != nil {
			return wc
		}
		return owned[0]
	}
	return disambiguateWorkspacesByPath(workspacePointers(cfg.Workspaces), absPath)
}

// pushWorkspaceErrHints 在执行方无法把收 push 的路由 id 映射回 workspace 时,给出可诊断
// 的报错:列出该配置下的 workspaces 及其 executor 绑定,便于核对「执行方配置是否已同步带
// 绑定」。最常见的成因是执行方 `relay watch` 读的还是旧配置(没有 `executor:` 绑定)——用
// `relay sync` 把共享配置推给执行方可解决。
func pushWorkspaceErrHints(cfg *config.Config, watchID, absPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "push-job: no workspace for push target=%q file=%q\n", watchID, absPath)
	if len(cfg.Workspaces) == 0 {
		b.WriteString("  (this config has no workspaces; run `relay sync` to write a shared config to this executor)\n")
		return b.String()
	}
	b.WriteString("  configured workspaces:\n")
	for _, w := range cfg.Workspaces {
		ex := w.Executor
		if ex == "" {
			ex = "(single-root)"
		}
		fmt.Fprintf(&b, "    - %s -> executor=%s\n", w.ID, ex)
	}
	fmt.Fprintf(&b, "  hint: the push target %q is an executor id; ensure at least one workspace behaves\n", watchID)
	b.WriteString("        bound to it (executor: <that id>) in the config THIS executor is running,\n")
	b.WriteString("        or use `relay sync -e <id>` to deliver the shared config then restart `relay watch`.\n")
	return b.String()
}

// workspacePointers 把值切片转成指针切片,便于统一按 *WorkspaceConfig 做路径消歧。
func workspacePointers(ws []config.WorkspaceConfig) []*config.WorkspaceConfig {
	out := make([]*config.WorkspaceConfig, len(ws))
	for i := range ws {
		out[i] = &ws[i]
	}
	return out
}

// disambiguateWorkspacesByPath 在候选 workspaces 里用落盘路径 absPath 挑选一个:
// 优先取 watch_dir 是 absPath 子串的;否则取路径段等于该 workspace id。
func disambiguateWorkspacesByPath(candidates []*config.WorkspaceConfig, absPath string) *config.WorkspaceConfig {
	segs := strings.Split(strings.ReplaceAll(absPath, "\\", "/"), "/")
	for _, wc := range candidates {
		// watch_dir 可能是相对子目录(如 "databus_backend")或绝对路径;两者都会以字符串形式出现在 absPath。
		if wc.WatchDir != "" && strings.Contains(absPath, wc.WatchDir) {
			return wc
		}
		for _, s := range segs {
			if s == wc.ID {
				return wc
			}
		}
	}
	return nil
}

func runExec() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	targetNode := resolveNodeTarget(*execNode, *execExec)
	if *execWatch != "" && targetNode != "" {
		fmt.Fprintln(os.Stderr, "Error: --node/--executor and --watch are mutually exclusive")
		os.Exit(1)
	}

	// 目标与 cwd: 若指定了 --node/--executor 则按节点身份寻址;
	// 否则必须定位到一个有效工作区(通过 -w 或当前工作目录匹配)
	var execCwd, targetExecutor string
	w := *execWatch
	if targetNode != "" {
		targetExecutor = targetNode
		w = ""
	} else {
		if w == "" {
			if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
				w = inferred
			}
		}
		if w == "" {
			fmt.Fprintf(os.Stderr, "Error: remote execution requires a workspace (specify -w or run within a configured workspace directory, or target node directly via -e/--node)\n")
			os.Exit(1)
		}
		watchCfg, err := cfg.GetWorkspaceByID(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		execCwd = watchCfg.LocalDir
		targetExecutor = watchCfg.Executor // 绑定 executor 时按其 executor_id 路由;空则单根回退
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	if !b.SupportsExec() {
		fmt.Fprintf(os.Stderr, "Backend does not support exec\n")
		os.Exit(1)
	}

	ctx := context.Background()

	// Get command directory, default to /tmp/relay-commands
	commandDir := "/tmp/relay-commands"
	if dir, ok := cfg.Backend.Config["command_dir"].(string); ok {
		commandDir = dir
	}

	// 健康检查:失败才打印,成功静默(不再每次输出 "Checking remote watcher... OK")。
	if w != "" {
		watchCfg, err := cfg.GetWorkspaceByID(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		if err := b.Ping(ctx, commandDir, watchCfg.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Remote watcher check failed: %v\n", err)
			os.Exit(1)
		}
	}

	// 增量同步准备:当未指定 --no-sync 时,检测本地对应工作区目录是否为 git 仓库且有 dirty 改动
	var gitSyncPayload *backend.GitSyncPayload
	if !*execNoSync {
		localSyncDir := execCwd
		if localSyncDir == "" {
			if curDir, err := os.Getwd(); err == nil {
				localSyncDir = curDir
			}
		}
		if localSyncDir != "" {
			repoInfo, err := git.InspectRepo(localSyncDir)
			if err == nil && repoInfo.IsGitRepo {
				// 本地是有效的 Git 仓库
				if repoInfo.IsDirty || *execForceRemote {
					payload, err := git.CreateDiffPayload(localSyncDir, *execForceRemote)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Git sync prepare failed: %v\n", err)
						os.Exit(1)
					}
					gitSyncPayload = &backend.GitSyncPayload{
						BaseCommit:       payload.BaseCommit,
						Patch:            payload.Patch,
						UntrackedArchive: payload.UntrackedArchive,
						Force:            payload.Force,
					}
					shortCommit := payload.BaseCommit
					if len(shortCommit) > 7 {
						shortCommit = shortCommit[:7]
					}
					fmt.Fprintf(os.Stderr, "➜ [git-sync] packaging dirty diff (base: %s, patch: %d B, untracked: %d B)\n",
						shortCommit, len(payload.Patch), len(payload.UntrackedArchive))
				}
			}
		}
	}

	cmd := strings.Join(*execCmdStr, " ")

	// 支持 Git 增量同步与后台托管的流式后端(relay)逐帧实时转发输出,并以 exit code 收尾
	if eb, ok := b.(backend.GitSyncExecBackend); ok {
		exit, err := eb.ExecStreamWithOptions(ctx, targetExecutor, cmd, execCwd, 0, gitSyncPayload, *execDetach, func(chunk backend.ExecChunk) {
			if chunk.Stdout {
				os.Stdout.WriteString(chunk.Data)
			} else {
				os.Stderr.WriteString(chunk.Data)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
			os.Exit(1)
		}
		if exit != 0 {
			os.Exit(exit)
		}
		return
	}

	// 支持流式的后端(relay)逐帧实时转发输出,并以 exit code 收尾
	if eb, ok := b.(backend.ExecStreamBackend); ok {
		exit, err := eb.ExecStream(ctx, targetExecutor, cmd, execCwd, 0, func(chunk backend.ExecChunk) {
			if chunk.Stdout {
				os.Stdout.WriteString(chunk.Data)
			} else {
				os.Stderr.WriteString(chunk.Data)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
			os.Exit(1)
		}
		if exit != 0 {
			os.Exit(exit)
		}
		return
	}

	result, err := b.Exec(ctx, cmd, execCwd, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result)
}

func runPs() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	tb, ok := b.(backend.TaskSupervisorBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Backend %q does not support task supervisor\n", cfg.Backend.Type)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetNode := resolveNodeTarget(*psNode, *psExec)
	tasks, err := tb.TaskList(ctx, targetNode, *psAll)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ps failed: %v\n", err)
		os.Exit(1)
	}

	if *psJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(tasks)
		return
	}

	if len(tasks) == 0 {
		if *psAll {
			fmt.Println("No tasks found.")
		} else {
			fmt.Println("No running tasks found (use -a to show all).")
		}
		return
	}

	fmt.Printf("%-12s %-16s %-10s %-6s %-16s %s\n", "TASK ID", "NODE", "STATUS", "EXIT", "CREATED", "COMMAND")
	for _, t := range tasks {
		createdStr := formatRelativeTime(time.Unix(t.StartTime, 0))
		exitStr := "-"
		if t.Status != "running" {
			exitStr = fmt.Sprintf("%d", t.ExitCode)
		}
		cmdDisp := t.Cmd
		if len(cmdDisp) > 40 {
			cmdDisp = cmdDisp[:37] + "..."
		}
		nodeDisp := t.Node
		if nodeDisp == "" {
			nodeDisp = "(unknown)"
		}
		fmt.Printf("%-12s %-16s %-10s %-6s %-16s %s\n", t.ID, nodeDisp, t.Status, exitStr, createdStr, cmdDisp)
	}
}

func formatRelativeTime(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func runLogs() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	tb, ok := b.(backend.TaskSupervisorBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Backend does not support task supervisor\n")
		os.Exit(1)
	}

	ctx := context.Background()
	targetNode := resolveNodeTarget(*logsNode, *logsExec)
	if targetNode == "" {
		if tasks, err := tb.TaskList(ctx, "", true); err == nil {
			for _, t := range tasks {
				if t.ID == *logsTaskID {
					targetNode = t.Node
					break
				}
			}
		}
	}
	err = tb.TaskLogs(ctx, targetNode, *logsTaskID, *logsFollow, *logsTail, func(chunk backend.ExecChunk) {
		if chunk.Stdout {
			os.Stdout.WriteString(chunk.Data)
		} else {
			os.Stderr.WriteString(chunk.Data)
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs failed: %v\n", err)
		os.Exit(1)
	}
}

func runStop() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	tb, ok := b.(backend.TaskSupervisorBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Backend does not support task supervisor\n")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetNode := resolveNodeTarget(*stopNode, *stopExec)
	if targetNode == "" {
		if tasks, err := tb.TaskList(ctx, "", true); err == nil {
			for _, t := range tasks {
				if t.ID == *stopTaskID {
					targetNode = t.Node
					break
				}
			}
		}
	}
	if err := tb.TaskStop(ctx, targetNode, *stopTaskID); err != nil {
		fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Stopped task %s\n", *stopTaskID)
}

// statusTimeout 是 `relay status` 单条命令的上限(含对中转 status 请求的等待)。
const statusTimeout = 15 * time.Second



// PeerNodeItem 描述拓扑中的一个对等节点
type PeerNodeItem struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	Platform string `json:"platform"`
	CPU      string `json:"cpu"`
	Memory   string `json:"memory"`
	Version  string   `json:"version"`
	Latency  string   `json:"latency"`
	Args     []string `json:"args,omitempty"`
}

// runNodes 查看全网节点拓扑状态与角色 (类似 kubectl get nodes / docker node ls)
func runNodes() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	rb, ok := b.(*relaybackend.RelayBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: nodes command requires relay backend, got %q\n", cfg.Backend.Type)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()

	withUsage := *nodesUsageOut
	st, err := rb.StatusWithUsage(ctx, withUsage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query cluster status: %v\n", err)
		os.Exit(1)
	}

	var nodes []PeerNodeItem

	// 1. 本机节点
	localPlatform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	localVer := ident(protocol.VersionInfo{Version: version.Version, Commit: version.Commit})
	localRoles := "client"
	if cfg.IsHub() && cfg.IsAgent() {
		localRoles = "hub,agent"
	} else if cfg.IsHub() {
		localRoles = "hub"
	} else if cfg.IsAgent() {
		localRoles = "agent"
	}

	localRes := sysinfo.CollectResourceInfo(withUsage)
	localCPU := localRes.FormatCPUSummary()
	localMem := localRes.FormatMemSummary()
	if withUsage {
		localCPU = localRes.FormatCPUUsage()
		localMem = localRes.FormatMemUsage()
	}

	nodes = append(nodes, PeerNodeItem{
		ID:       cfg.NodeID() + " (local)",
		Role:     localRoles,
		Status:   "Ready",
		Platform: localPlatform,
		CPU:      localCPU,
		Memory:   localMem,
		Version:  localVer,
		Latency:  "-",
	})

	// 2. Hub / Transit 节点
	transitPlatform := "-"
	if st.Transit.GOOS != "" {
		transitPlatform = fmt.Sprintf("%s/%s", st.Transit.GOOS, st.Transit.GOARCH)
	}
	transitSysRes := sysinfo.ResourceInfo{
		CPUCores:    st.Resource.CPUCores,
		CPULoad:     st.Resource.CPULoad,
		MemTotalMB:  st.Resource.MemTotalMB,
		MemUsedMB:   st.Resource.MemUsedMB,
		IsContainer: st.Resource.IsContainer,
	}
	transitCPU := transitSysRes.FormatCPUSummary()
	transitMem := transitSysRes.FormatMemSummary()
	if withUsage {
		transitCPU = transitSysRes.FormatCPUUsage()
		transitMem = transitSysRes.FormatMemUsage()
	}

	transitRole := "hub"
	hasHubAgent := false
	for _, e := range st.Executors {
		if e.ExecutorID == "hub" {
			hasHubAgent = true
			break
		}
	}
	if hasHubAgent {
		transitRole = "hub, agent"
	}

	nodes = append(nodes, PeerNodeItem{
		ID:       "hub (transit)",
		Role:     transitRole,
		Status:   "Ready",
		Platform: transitPlatform,
		CPU:      transitCPU,
		Memory:   transitMem,
		Version:  ident(st.Transit),
		Latency:  segmentText(st.Seg1),
		Args:     st.Transit.Args,
	})

	// 3. 远端 Agent / Executor 节点
	for _, e := range st.Executors {
		if e.ExecutorID == "hub" {
			// hub 自身作为 agent 已合并在 hub (transit) 呈现，避免重复行
			continue
		}
		stText := "Ready"
		if e.Seg2.Unavailable != "" {
			stText = "NotReady"
		}
		platform := "-"
		if e.OS != "" {
			platform = fmt.Sprintf("%s/%s", e.OS, e.Arch)
		}

		execSysRes := sysinfo.ResourceInfo{
			CPUCores:    e.Resource.CPUCores,
			CPULoad:     e.Resource.CPULoad,
			MemTotalMB:  e.Resource.MemTotalMB,
			MemUsedMB:   e.Resource.MemUsedMB,
			IsContainer: e.Resource.IsContainer,
		}
		execCPU := execSysRes.FormatCPUSummary()
		execMem := execSysRes.FormatMemSummary()
		if withUsage {
			execCPU = execSysRes.FormatCPUUsage()
			execMem = execSysRes.FormatMemUsage()
		}

		nodes = append(nodes, PeerNodeItem{
			ID:       e.ExecutorID,
			Role:     "agent",
			Status:   stText,
			Platform: platform,
			CPU:      execCPU,
			Memory:   execMem,
			Version:  identStr(e.Version),
			Latency:  segmentText(e.Total),
		})
	}

	if *nodesJSONOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(nodes)
		return
	}

	// 文本表格输出 (类似 kubectl get nodes)
	if withUsage {
		fmt.Printf("%-24s %-10s %-8s %-14s %-16s %-24s %-16s %s\n", "NAME", "ROLES", "STATUS", "PLATFORM", "CPU", "MEMORY", "VERSION", "LATENCY")
		for _, n := range nodes {
			fmt.Printf("%-24s %-10s %-8s %-14s %-16s %-24s %-16s %s\n", n.ID, n.Role, n.Status, n.Platform, n.CPU, n.Memory, n.Version, n.Latency)
		}
	} else {
		fmt.Printf("%-24s %-10s %-8s %-14s %-12s %-14s %-16s %s\n", "NAME", "ROLES", "STATUS", "PLATFORM", "CPU", "MEMORY", "VERSION", "LATENCY")
		for _, n := range nodes {
			fmt.Printf("%-24s %-10s %-8s %-14s %-12s %-14s %-16s %s\n", n.ID, n.Role, n.Status, n.Platform, n.CPU, n.Memory, n.Version, n.Latency)
		}
	}
}

// identStr 把版本号渲染成标识(空版本给 unknown)。
func identStr(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// ident 把 VersionInfo 渲染成可用于对比的标识符(带 commit 时版+commit)。
func ident(n protocol.VersionInfo) string {
	if n.Commit != "" {
		return n.Version + "+" + n.Commit
	}
	return n.Version
}

// segmentText 渲染一段时延;可用的给毫秒,不可用的给括号原因。
func segmentText(s protocol.StatusSegment) string {
	if s.LatencyMS != nil {
		return fmt.Sprintf("%d ms", *s.LatencyMS)
	}
	if s.Unavailable != "" {
		return "N/A (" + s.Unavailable + ")"
	}
	return "N/A"
}

// runVersion 打印纯本机构建信息。跨机版本台账职责已归一到 `relay status`;`--json` 仍保留。
func runVersion() {
	if *versionJSONOut {
		rep := map[string]interface{}{
			"local": map[string]interface{}{
				"version": version.Version,
				"commit":  version.Commit,
				"build":   version.Date,
				"goos":    runtime.GOOS,
				"goarch":  runtime.GOARCH,
				"go":      runtime.Version(),
			},
		}
		enc, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(enc))
		return
	}

	fmt.Printf("relay %s\n", version.Full())
	fmt.Printf("  local  %s/%s  commit=%s  built=%s  go=%s\n",
		runtime.GOOS, runtime.GOARCH, orDash(version.Commit), orDash(version.Date), runtime.Version())
	fmt.Println("  (远端台账见 `relay status`)")
}

// runServerRemote 一键部署中转:读取本地 relay 二进制,经 `server-remote` 受控自升级
// 发到中转(自检/换装在服务端内完成),等待成功 ACK 后断线重连、轮询版本台账核验。
// 失败时非零退出并给出人工回退提示(中转保留 .prev 备件)。
func runServerRemote() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: load config: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: create backend: %v\n", err)
		os.Exit(1)
	}
	rb, ok := b.(*relaybackend.RelayBackend)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: server-remote requires relay backend (backend.type=relay), got %q\n", cfg.Backend.Type)
		os.Exit(1)
	}

	bin := *serverRemoteBin
	if bin == "" {
		if exe, err := os.Executable(); err == nil {
			bin = exe
		}
	}
	if bin == "" {
		fmt.Fprintf(os.Stderr, "Error: cannot determine binary to send; pass --binary\n")
		os.Exit(1)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: read binary %s: %v\n", bin, err)
		os.Exit(1)
	}

	fmt.Printf("[server-remote] 上传 %s (%d bytes) 到中转并触发自升级 ...\n", bin, len(data))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := rb.UpgradeServer(ctx, data); err != nil {
		fmt.Fprintf(os.Stderr, "Error: server upgrade failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "提示: 中转已保留 .prev 备件;请人工回退(如经 code-server 上传 + `relay server upgrade`)后再重试。\n")
		os.Exit(1)
	}

	// 期望版本:显式 --expect 优先;否则回退本进程 version.String()。换装后中转跑的应是
	// --binary 那份二进制的构建标识——若它与本地进程版本不一致(交叉编译 / 树带 -dirty),
	// 必须显式传入,否则对账永远无法达成而超时。
	want := *serverRemoteExpect
	if want == "" {
		want = version.String()
	}
	fmt.Println("[server-remote] 中转已回 ACK(自检通过); 正在轮询版本核验 ...")
	if err := pollTransitVersion(rb, want); err != nil {
		fmt.Fprintf(os.Stderr, "Error: 版本核验失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[server-remote] 中转已运行新版本 ✔")
}

// pollTransitVersion 轮询中转版本台账,等待其 transit 节点构建与本地一致。中转换装会
// 短时断连,这里经重连持续轮询,直到对账一致或超时。
func pollTransitVersion(rb *relaybackend.RelayBackend, want string) error {
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error = fmt.Errorf("no transit node matched yet")
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		vr, verr := rb.Version(ctx)
		cancel()
		if verr == nil {
			found := false
			for _, n := range vr.Nodes {
				if n.Role == "transit" {
					found = true
					got := n.Version
					if n.Commit != "" {
						got = n.Version + "+" + n.Commit
					}
					if got == want {
						return nil
					}
					lastErr = fmt.Errorf("transit 版本 %s != 本地 %s", got, want)
				}
			}
			if !found {
				lastErr = fmt.Errorf("台账中无 transit 节点(中转可能仍不可达)")
			}
		} else {
			lastErr = verr
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out awaiting transit 与本地版本一致: %w", lastErr)
}

// orDash 空串显示为 "-"。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runJobRun() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchCfg, err := lookupWatch(cfg, *jobRunWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[job] ▶ %s (%s)\n", *jobRunID, jobTypeOf(watchCfg, *jobRunID))
	res, err := jobrunner.Run(context.Background(), watchCfg, *jobRunID, *jobRunFile)
	if err != nil {
		// The error already carries the captured stderr (for exec failures), so
		// print only the error and exit non-zero; res.Stderr would repeat it.
		fmt.Fprintf(os.Stderr, "Job error: %v\n", err)
		fmt.Fprintf(os.Stderr, "[job] ✘ %s failed\n", *jobRunID)
		os.Exit(1)
	}

	if res.Stdout != "" {
		fmt.Print(res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	fmt.Printf("[job] ✔ %s ok (exit=%d, %s)\n", *jobRunID, res.ExitCode, res.Duration.Round(time.Millisecond))
}

// jobTypeOf 返回给定 workspace 内 jobID 的类型,便于 job run 回显 step 标记;
// 找不到时返回空串(不影响正常执行输出)。
func jobTypeOf(watchCfg *config.WorkspaceConfig, jobID string) string {
	for i := range watchCfg.Jobs {
		if watchCfg.Jobs[i].ID == jobID {
			return watchCfg.Jobs[i].Type
		}
	}
	return ""
}

func pushFile(ctx context.Context, b backend.FileTransferBackend, src, dest string) {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read source file: %v\n", err)
		os.Exit(1)
	}

	if err := b.Write(ctx, dest, data); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write to remote: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushed: %s -> %s\n", src, dest)
}

func pushDir(ctx context.Context, b backend.FileTransferBackend, src, dest string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read source directory: %v\n", err)
		os.Exit(1)
	}

	if dest != "" && !strings.HasSuffix(dest, "/") {
		dest += "/"
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		destPath := dest + entry.Name()

		if entry.IsDir() {
			pushDir(ctx, b, srcPath, destPath)
		} else {
			pushFile(ctx, b, srcPath, destPath)
		}
	}
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func runCleanup() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	_, err = cfg.GetWorkspaceByID(*cleanupWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	// 与 exec/heartbeat/命令处理统一默认(见其它端点),否则 relay/fs-mcp 模式下
	// cleanup 会去 /commands 扑空。
	commandDir := "/tmp/relay-commands"
	if dir, ok := cfg.Backend.Config["command_dir"].(string); ok {
		commandDir = dir
	}

	files, err := b.ListDir(ctx, commandDir)
	if err != nil {
		// Directory doesn't exist or other error - nothing to clean
		fmt.Println("No command files to clean up (command directory may not exist)")
		return
	}

	var cleaned int
	for _, f := range files {
		if strings.HasPrefix(f.Name, "cmd-") || strings.HasPrefix(f.Name, "result-") {
			filePath := commandDir + "/" + f.Name
			if err := b.Delete(ctx, filePath); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to delete %s: %v\n", f.Name, err)
			} else {
				cleaned++
				fmt.Printf("Deleted: %s\n", f.Name)
			}
		}
	}

	if cleaned == 0 {
		fmt.Println("No stale command files found")
	} else {
		fmt.Printf("Cleanup complete: %d files removed\n", cleaned)
	}
}

func runSync() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Get command directory
	commandDir := "/tmp/relay-commands"
	if dir, ok := cfg.Backend.Config["command_dir"].(string); ok && dir != "" {
		commandDir = dir
	}

	// Read local config file
	configData, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read config file: %v\n", err)
		os.Exit(1)
	}

	// Expand environment variables in config content
	configData = []byte(os.ExpandEnv(string(configData)))

	// Create backend for file exchange
	b, err := getBackendWithAdaptiveLoopback(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	// 通道遵循 backend:支持流式的后端(relay)经 WS 直达执行端落盘,不绕中转文件交换。
	// --node/--executor 指定目标执行方;空=单根回退到本配置的 backend.executor_id。
	if cs, ok := b.(backend.ConfigSyncCapable); ok {
		target := resolveNodeTarget(*syncNode, *syncExecutor)
		if target != "" {
			fmt.Printf("Syncing config to node %q via %s streaming channel...\n", target, cfg.Backend.Type)
		} else {
			fmt.Printf("Syncing config via %s streaming channel...\n", cfg.Backend.Type)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		exit, err := cs.ConfigSync(ctx, target, configData)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Sync failed: %v\n", err)
			os.Exit(1)
		}
		if exit != 0 {
			fmt.Fprintf(os.Stderr, "Sync failed on executor (exit %d)\n", exit)
			os.Exit(1)
		}
		fmt.Println("Sync completed (config written on executor; restart relay watch to take effect)")
		return
	}

	// Build config-sync command
	cmdFile := exchange.BuildConfigSyncCmd(configData)
	cmdPath := "cmd-" + cmdFile.ID + ".json"
	fullCmdPath := commandDir + "/" + cmdPath

	// Write command file
	cmdData, err := json.Marshal(cmdFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal command: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushing config to watcher (command_dir: %s)...\n", commandDir)
	if err := b.Write(context.Background(), fullCmdPath, cmdData); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write command file: %v\n", err)
		os.Exit(1)
	}

	// Poll for result
	resultPath := "result-" + cmdFile.ID + ".json"
	fullResultPath := commandDir + "/" + resultPath
	pollInterval := 2 * time.Second
	timeout := 300 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "Timeout waiting for watcher response\n")
			os.Exit(1)
		case <-ticker.C:
			data, err := b.Read(context.Background(), fullResultPath)
			if err != nil {
				continue
			}

			var result exchange.ResultFile
			if err := json.Unmarshal(data, &result); err != nil {
				continue
			}

			if result.ID != cmdFile.ID {
				continue
			}

			// Got result - cleanup and report
			_ = b.Delete(context.Background(), fullCmdPath)
			_ = b.Delete(context.Background(), fullResultPath)

			if result.ExitCode != 0 {
				fmt.Fprintf(os.Stderr, "Sync failed: %s\n", result.Stderr)
				os.Exit(1)
			}

			fmt.Printf("Sync successful: %s\n", result.Stdout)
			return
		}
	}
}
