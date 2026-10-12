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

	// 子命令定义
	_                = watchCmd.Command("run", "Run watcher in foreground")
	_                = watchCmd.Command("start", "Start watcher daemon in background")
	_                = watchCmd.Command("stop", "Stop running watcher daemon")
	_                = watchCmd.Command("status", "Show status of watcher daemon")
	_                = watchCmd.Command("restart", "Restart watcher daemon")
	watchUpgradeCmd  = watchCmd.Command("upgrade", "Upgrade watcher daemon binary")
	watchUpgradePath = watchUpgradeCmd.Arg("upgrade-path", "Path to new relay binary (with action=upgrade)").String()

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
	execPublish     = execCmd.Flag("publish", "Publish remote port to local (e.g. -p 8080 or -p 8080:8080, similar to docker run -p)").Short('p').Strings()
	execBindHost    = execCmd.Flag("bind-host", "Local bind host for published ports (default: 127.0.0.1)").Default("127.0.0.1").String()
	// -t/--type 选择创建的对象:缺省 task(在远端跑命令);tunnel 则在本机 node 上开一条 SOCKS5 隧道。
	execType = execCmd.Flag("type", "Object type to create: task (default) or tunnel").Short('t').String()
	// --restart/--no-restart 是任务与隧道共用的重启策略旗标对(隧道模式写入隧道意图 R14;
	// 任务的写入/校验由任务模式实现负责)。校验抽成 restartPolicyValue 纯函数共用。
	execRestartPolicy = execCmd.Flag("restart", "Restart policy on node restart: always|no").Default("no").String()
	execNoRestart     = execCmd.Flag("no-restart", "Shortcut for --restart=no").Bool()
	// 支持多词命令:relay run git status 会收成 ["git","status"] 再空格 join。
	// 含以 - 开头参数的命令(如 git log --oneline)仍须整体加引号,避免被当 flag。
	// 不设 Required:模式按 KTD7 由代码校验(任务模式必需,隧道模式必须为空)。
	execCmdStr = execCmd.Arg("command", "Command to execute (multiple words are joined; required in task mode)").Strings()

	// ps command - 查询后台任务
	psCmd  = kingpin.Command("ps", "List managed background tasks and node-held tunnels (similar to docker ps)")
	psAll  = psCmd.Flag("all", "Show all tasks (default shows running only)").Short('a').Bool()
	psExec = psCmd.Flag("executor", "Filter tasks by executor node").Short('e').HintAction(getNodeHints).String()
	psNode = psCmd.Flag("node", "Filter tasks by node ID (alias for --executor)").HintAction(getNodeHints).String()
	psJSON = psCmd.Flag("json", "Output as JSON").Bool()

	// logs command - 查看任务日志
	logsCmd    = kingpin.Command("logs", "Tail or stream background task logs, or a node-held tunnel's log (similar to docker logs)")
	logsTaskID = logsCmd.Arg("task-id", "Task ID or tunnel egress to view").Required().String()
	logsFollow = logsCmd.Flag("follow", "Follow log output").Short('f').Bool()
	logsTail   = logsCmd.Flag("tail", "Number of lines to show from the end").Default("100").Int()
	logsExec   = logsCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	logsNode   = logsCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	// stop command - 终止后台任务
	stopCmd    = kingpin.Command("stop", "Terminate a running task process tree or a node-held tunnel (similar to docker stop)")
	stopTaskID = stopCmd.Arg("task-id", "Task ID or tunnel egress to stop").Required().String()
	stopExec   = stopCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	stopNode   = stopCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	// Nodes command - 查看全网节点拓扑状态与角色 (类似 kubectl get nodes / docker node ls; alias: status)
	nodesCmd      = kingpin.Command("nodes", "List cluster peer nodes and topology (alias: status)").Alias("status")
	nodesJSONOut  = nodesCmd.Flag("json", "Output as JSON").Bool()
	nodesUsageOut = nodesCmd.Flag("usage", "Query dynamic CPU load & memory usage of nodes (heavier probe)").Short('u').Bool()
	nodesPrune    = nodesCmd.Flag("prune-offline", "Remove known-offline executors from the transit ledger").Bool()

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
	serverRemoteCmd    = kingpin.Command("server-remote", "Deploy and self-upgrade transit server remotely")
	serverRemoteBin    = serverRemoteCmd.Flag("binary", "Path to the new relay binary to send (default: current executable)").String()
	serverRemoteExpect = serverRemoteCmd.Flag("expect", "Expected transit version after swap (default: this relay's version.String())").String()

	tunnelCmd      = kingpin.Command("tunnel", "Open SOCKS5 egress tunnel through remote node")
	tunnelListen   = tunnelCmd.Flag("listen", "Local SOCKS5 listen address").Default("127.0.0.1:1080").String()
	tunnelExecName = tunnelCmd.Flag("executor", "Egress executor's executor_id (see `relay status` -> executors[].executor_id)").Short('w').HintAction(getNodeHints).String()
	tunnelNodeName = tunnelCmd.Flag("node", "Egress node ID (alias for --executor)").HintAction(getNodeHints).String()

	// tunnel 子命令:隧道由本地 node 持有(类比 dockerd),CLI 仅作控制面经 unix socket 下发
	// open/close/restart/list。故没有 run(常驻进程已并入 node),也没有独立的 logs/upgrade/status
	// —— 读取统一走 `relay ps`(TYPE tunnel 行)与 `relay logs <egress>`,tunnel 侧只留写动词。
	tunnelStartCmd      = tunnelCmd.Command("start", "Start a SOCKS5 tunnel held by the local node")
	tunnelRestartPolicy = tunnelStartCmd.Flag("restart", "Restart policy on node restart: always|no").Default("no").String()
	tunnelNoRestart     = tunnelStartCmd.Flag("no-restart", "Shortcut for --restart=no").Bool()
	_                   = tunnelCmd.Command("stop", "Stop a node-held tunnel")
	_                   = tunnelCmd.Command("restart", "Restart a node-held tunnel (reopen without retyping flags)")

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

	parseCtx, parseErr := kingpin.CommandLine.ParseContext(os.Args[1:])
	if parseErr == nil && parseCtx != nil {
		if parseCtx.SelectedCommand == nil || len(parseCtx.SelectedCommand.Model().Commands) > 0 {
			kingpin.CommandLine.UsageForContext(parseCtx)
			return
		}
	}

	selectedCmd := kingpin.Parse()
	switch {
	case selectedCmd == "server" || strings.HasPrefix(selectedCmd, "server "):
		sub := strings.TrimSpace(strings.TrimPrefix(selectedCmd, "server"))
		switch sub {
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
		case "run":
			if err := runServer(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		default:
			app.Usage(os.Args[1:])
		}
	case selectedCmd == "node" || strings.HasPrefix(selectedCmd, "node "):
		sub := strings.TrimSpace(strings.TrimPrefix(selectedCmd, "node"))
		switch sub {
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
		case "run":
			if err := runNode(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		default:
			app.Usage(os.Args[1:])
		}
	case selectedCmd == "watch" || strings.HasPrefix(selectedCmd, "watch "):
		sub := strings.TrimSpace(strings.TrimPrefix(selectedCmd, "watch"))
		switch sub {
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
		case "run":
			runWatch()
		default:
			app.Usage(os.Args[1:])
		}
	case selectedCmd == "tunnel" || strings.HasPrefix(selectedCmd, "tunnel "):
		sub := strings.TrimSpace(strings.TrimPrefix(selectedCmd, "tunnel"))
		switch sub {
		case "start":
			if err := runTunnelStart(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		case "stop":
			runTunnelStop()
		case "restart":
			if err := runTunnelRestart(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		default:
			app.Usage(os.Args[1:])
		}
	case selectedCmd == pullCmd.FullCommand():
		runPull()
	case selectedCmd == pushCmd.FullCommand():
		runPush()
	case selectedCmd == execCmd.FullCommand():
		runExec()
	case selectedCmd == nodesCmd.FullCommand():
		runNodes()
	case selectedCmd == listCmd.FullCommand():
		runList()
	case selectedCmd == cleanupCmd.FullCommand():
		runCleanup()
	case selectedCmd == "help":
		app.Usage(os.Args[1:])
	case selectedCmd == syncCmd.FullCommand():
		runSync()
	case selectedCmd == wsCmd.FullCommand():
		runWorkspaces()
	case selectedCmd == versionCmd.FullCommand():
		runVersion()
	case selectedCmd == psCmd.FullCommand():
		runPs()
	case selectedCmd == logsCmd.FullCommand():
		runLogs()
	case selectedCmd == stopCmd.FullCommand():
		runStop()
	case selectedCmd == serverRemoteCmd.FullCommand():
		runServerRemote()
	case selectedCmd == completionCmd.FullCommand():
		runCompletion()
	case selectedCmd == jobRun.FullCommand():
		runJobRun()
	case selectedCmd == undoCmd.FullCommand():
		runUndo()
	case selectedCmd == diffCmd.FullCommand():
		runDiff()
	default:
		app.Usage(os.Args[1:])
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

// tunnelInstanceName 由 executor 派生旧式独立隧道 daemon 的实例名(pid/log 文件名用,形如
// `tunnel-<executor>`)。隧道现已由本地 node 持有,该名字仅用于识别并清理残留的旧 daemon
// (它们可能仍占着监听地址);executor 是执行方注册的身份,可能含非法文件名字符,做净化后作后缀。
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

// tunnelRequireExecutor 校验隧道控制类操作(启动/停止/重启/升级)必须携带 node/executor 并返回目标节点 ID。
func tunnelRequireExecutor() (string, error) {
	target := resolveNodeTarget(*tunnelNodeName, *tunnelExecName)
	if target == "" {
		return "", fmt.Errorf("Specify --node or --executor with the egress node ID (see `relay status` -> `executors[].executor_id`)")
	}
	return target, nil
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
	defer relaybackend.Cleanup()

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

// tunnelModeInput 是 relay run 在隧道模式下用到的已解析旗标。显式传入(不读全局变量),
// 使模式/旗标校验成为可逐条断言的纯决策(KTD7)。
type tunnelModeInput struct {
	Command   []string
	Publish   []string
	BindHost  string
	Watch     string
	Node      string
	Executor  string
	Detach    bool
	NoSync    bool
	Force     bool
	NoRestart bool
	Restart   string
}

// tunnelModeRequest 是隧道模式归一后的创建请求。
type tunnelModeRequest struct {
	Listen  *TunnelListenSpec
	Egress  string
	Restart string
}

// resolveTunnelMode 按模式矩阵校验并归一 relay run --type=tunnel 的旗标(R14/R16):
// 恰好一个 -p([host:]port,无远端半段),没有位置命令,不接受任务语义旗标(-d/--no-sync/-f/-w),
// 出口取 -e/--node,策略取 --restart/--no-restart。任一违规组合显式报错,不静默忽略。
func resolveTunnelMode(in tunnelModeInput) (*tunnelModeRequest, error) {
	if len(in.Command) > 0 {
		return nil, fmt.Errorf("--type=tunnel takes no command (got %q); tunnels are created by the local node, not by running a command", strings.Join(in.Command, " "))
	}
	if in.Detach {
		return nil, fmt.Errorf("-d/--detach is task-only and cannot be combined with --type=tunnel (tunnels are held by the local node)")
	}
	if in.NoSync {
		return nil, fmt.Errorf("--no-sync is task-only and cannot be combined with --type=tunnel")
	}
	if in.Force {
		return nil, fmt.Errorf("-f/--force-remote is task-only and cannot be combined with --type=tunnel")
	}
	if in.Watch != "" {
		return nil, fmt.Errorf("-w/--watch is task-only and cannot be combined with --type=tunnel; pick the tunnel egress with -e/--executor")
	}
	switch len(in.Publish) {
	case 0:
		return nil, fmt.Errorf("--type=tunnel requires exactly one -p <[host:]port>")
	case 1:
	default:
		return nil, fmt.Errorf("--type=tunnel accepts exactly one -p, got %d", len(in.Publish))
	}
	listen, err := parseTunnelListenSpec(in.Publish[0], in.BindHost)
	if err != nil {
		return nil, err
	}
	egress := resolveNodeTarget(in.Node, in.Executor)
	if egress == "" {
		return nil, fmt.Errorf("Specify --node or --executor with the egress node ID (see `relay status` -> `executors[].executor_id`)")
	}
	policy, err := restartPolicyValue(in.Restart, in.NoRestart)
	if err != nil {
		return nil, err
	}
	return &tunnelModeRequest{Listen: listen, Egress: egress, Restart: policy}, nil
}

// requireTaskCommand 校验任务模式必须有位置命令。kingpin 层不再声明 Required,
// 改为按模式校验(KTD7:隧道模式必须没有命令,任务模式必须有)。
func requireTaskCommand(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("a command is required (e.g. `relay run echo hi`); use --type=tunnel to create a tunnel instead")
	}
	return nil
}

func runExec() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// 模式分支(KTD7):置于工作区/执行方解析之前,隧道模式直接走创建流程,不解析工作区,
	// 也不需要命令;任务模式(默认)保持既有行为。两种路径的旗标规则互不干扰。
	switch mode := strings.ToLower(strings.TrimSpace(*execType)); mode {
	case "", "task":
		if err := requireTaskCommand(*execCmdStr); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "tunnel":
		req, err := resolveTunnelMode(tunnelModeInput{
			Command:   *execCmdStr,
			Publish:   *execPublish,
			BindHost:  *execBindHost,
			Watch:     *execWatch,
			Node:      *execNode,
			Executor:  *execExec,
			Detach:    *execDetach,
			NoSync:    *execNoSync,
			Force:     *execForceRemote,
			NoRestart: *execNoRestart,
			Restart:   *execRestartPolicy,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := startTunnel(cfg, req.Egress, req.Listen.Addr(), req.Restart, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid --type %q (want task|tunnel)\n", *execType)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// 端口转发准备 (-p / --publish)
	var pfSpecs []*PortForwardSpec
	if len(*execPublish) > 0 {
		for _, p := range *execPublish {
			spec, err := parsePortForwardSpec(p, *execBindHost)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid port-forward spec %q: %v\n", p, err)
				os.Exit(1)
			}
			pfSpecs = append(pfSpecs, spec)
		}
	}

	if len(pfSpecs) > 0 && !*execDetach {
		rb, ok := b.(*relaybackend.RelayBackend)
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: port-forward requires relay backend\n")
			os.Exit(1)
		}
		stopPF, err := StartPortForwardsForeground(ctx, rb, targetExecutor, pfSpecs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Port-forward error: %v\n", err)
			os.Exit(1)
		}
		defer stopPF()
	}

	// detach 模式 + 端口转发:确保本地 node daemon(端口转发持有者)运行并等待其控制 socket 就绪。
	// 与前台 -p 不同,这里本地监听由常驻 node 维持而非当前 CLI 进程。
	var detachTaskID string
	if len(pfSpecs) > 0 && *execDetach {
		if err := ensureLocalRelayNode(cfg, forwardSockPath()); err != nil {
			fmt.Fprintf(os.Stderr, "Port-forward error: %v\n", err)
			os.Exit(1)
		}
	}

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

	// 增量同步准备:当未指定 --no-sync 时,检测本地对应工作区目录是否为 git 仓库且有 dirty 改动。
	// 仅在工作区已解析(w != "")时同步——同步源是工作区的 local_dir。
	// 直接按节点寻址(--executor/--node,无 -w)时没有工作区,也就没有有意义的同步源;
	// 此时回落到 os.Getwd()(如部署脚本在仓库根执行)+ 远端 execDir 会把本地脏 diff 强推到
	// 无关目录,产生 "is not a git repository on target node" 之类误报,故跳过。
	var gitSyncPayload *backend.GitSyncPayload
	if !*execNoSync && w != "" {
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
		var portStrs []string
		for _, spec := range pfSpecs {
			portStrs = append(portStrs, spec.String())
		}
		exit, err := eb.ExecStreamWithPorts(ctx, targetExecutor, cmd, execCwd, 0, gitSyncPayload, *execDetach, portStrs, func(chunk backend.ExecChunk) {
			if chunk.Stdout {
				// detach 时从 stdout 捕获任务 ID(marker 前缀精确解析)。
				if *execDetach && detachTaskID == "" {
					if id, ok := captureDetachTaskID(chunk.Data); ok {
						detachTaskID = id
						return // 标记行不回显给用户 stdout
					}
				}
				os.Stdout.WriteString(chunk.Data)
			} else {
				os.Stderr.WriteString(chunk.Data)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
			os.Exit(1)
		}
		// detach + 端口转发:远端任务已启动,向本地 node 注册端口转发以维持常驻监听。
		if *execDetach && len(pfSpecs) > 0 {
			if detachTaskID == "" {
				// 未捕获任务 ID 是硬故障:远端任务已 detach,本地端口将不可达,静默 exit 0 会误导用户。
				fmt.Fprintf(os.Stderr, "Error: failed to capture task ID from remote; local port-forward not registered\n")
				os.Exit(1)
			}
			registered, rejected, regErr := sendForwardRegister(forwardSockPath(), detachTaskID, targetExecutor, pfSpecs)
			if regErr != nil {
				fmt.Fprintf(os.Stderr, "Port-forward register error: %v\n", regErr)
				os.Exit(1)
			}
			for _, a := range registered {
				fmt.Printf("➜ [port-forward] %s still listening (task %s)\n", a, detachTaskID)
			}
			for _, r := range rejected {
				fmt.Fprintf(os.Stderr, "WARNING: %s (task %s)\n", r, detachTaskID)
			}
			// 已输出转发行,不再重复 echo task ID(转发行内已含 task id)。
		} else if *execDetach && detachTaskID != "" {
			// detach 无 -p 时,CLI 需把解析出的任务 ID 回显给用户(--detach 承诺返回 TaskID)。
			fmt.Println(detachTaskID)
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

// psRow 是 relay ps 的统一行:本地 node 持有的隧道 + 远端 detach 任务合并呈现。
type psRow struct {
	Type    string `json:"type"` // "task" | "tunnel"
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"`
	Status  string `json:"status"`
	Ports   string `json:"ports,omitempty"`   // task: comma-joined; tunnel: the listen address
	Exit    string `json:"exit,omitempty"`    // task: exit code or "-"
	Created string `json:"created,omitempty"` // 相对时间("3m ago")
	Command string `json:"command"`           // task: cmd; tunnel: "egress <egress>"
	Egress  string `json:"egress,omitempty"`  // tunnel only
	Restart string `json:"restart,omitempty"` // tunnel only
	// 隧道出口就绪(取自中转台账,与 `relay nodes` 同一事实来源)。出口是 SOCKS5 的三段链路末段,
	// 本地 listener 活着不代表出口活着,故单独成列/字段。
	EgressStatus string `json:"egress_status,omitempty"` // ready | offline | notready | unknown
	EgressReady  *bool  `json:"egress_ready,omitempty"`  // nil = 未探测或未知

	// statusDisplay 仅供表格:STATUS + 出口就绪。JSON 的 status 保持纯状态值(running/stopped),
	// 免得机器消费者去解析人类文案。
	statusDisplay string
}

// displayStatus 返回表格用的 STATUS 单元格:叠加了出口就绪时用叠加文案,否则用纯 status。
func (r psRow) displayStatus() string {
	if r.statusDisplay != "" {
		return r.statusDisplay
	}
	return r.Status
}

// taskToPSRow 把一个远端 detach 任务投影为统一行。纯函数,便于单测。
// 字段裁剪复刻旧 runPs 行为:Node 空 → (unknown),Cmd 超 35 字符 → 前 32 + "..."。
func taskToPSRow(t backend.TaskInfo) psRow {
	nodeDisp := t.Node
	if nodeDisp == "" {
		nodeDisp = "(unknown)"
	}
	portsDisp := "-"
	if len(t.Ports) > 0 {
		portsDisp = strings.Join(t.Ports, ",")
		if len(portsDisp) > 16 {
			portsDisp = portsDisp[:13] + "..."
		}
	}
	exitDisp := "-"
	if t.Status != "running" {
		exitDisp = fmt.Sprintf("%d", t.ExitCode)
	}
	cmdDisp := t.Cmd
	if len(cmdDisp) > 35 {
		cmdDisp = cmdDisp[:32] + "..."
	}
	return psRow{
		Type:    "task",
		ID:      t.ID,
		Node:    nodeDisp,
		Status:  t.Status,
		Ports:   portsDisp,
		Exit:    exitDisp,
		Created: formatRelativeTime(time.Unix(t.StartTime, 0)),
		Command: cmdDisp,
	}
}

// tunnelToPSRow 把本地 node 持有的一条隧道投影为统一行。纯函数,便于单测。
func tunnelToPSRow(ts tunnelState) psRow {
	createdDisp := "-"
	if !ts.Started.IsZero() {
		createdDisp = formatRelativeTime(ts.Started)
	}
	return psRow{
		Type:    "tunnel",
		ID:      ts.Egress,
		Node:    ts.Egress,
		Status:  ts.State,
		Ports:   ts.Listen,
		Exit:    "-",
		Created: createdDisp,
		Command: "egress " + ts.Egress,
		Egress:  ts.Egress,
		Restart: ts.Restart,
	}
}

// egressState 是一条隧道出口 executor 的在线就绪快照,取自中转全网台账的 ExecutorHealth。
type egressState struct {
	Ready   bool
	Text    string // ready | offline | notready
	Latency string // 仅 ready 时非空,如 "11ms"
}

// compactLatency 只取延迟数字:STATUS 列是一个窄单元格,不可复用 segmentText 的
// "N/A (executor offline)" 这类长降级文案。
func compactLatency(s protocol.StatusSegment) string {
	if s.LatencyMS == nil {
		return ""
	}
	return fmt.Sprintf("%dms", *s.LatencyMS)
}

// queryEgressStates 向中转查询全网节点台账,返回 executor_id -> 出口就绪快照。判定与 `relay nodes`
// 完全一致(离线看 LastSeen;Seg2 不可用=NotReady)。1.5s 上限、任何错误静默降级为 nil——调用方
// 据此把隧道行标成 unknown,而不是让 `relay ps` 因为探测失败而失败。
func queryEgressStates(cfg *config.Config) map[string]egressState {
	rb, err := getRelayBackend(cfg)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	st, err := rb.StatusWithUsage(ctx, false)
	if err != nil || !st.OK {
		return nil
	}
	out := make(map[string]egressState, len(st.Executors))
	for _, e := range st.Executors {
		out[e.ExecutorID] = egressStateOf(e)
	}
	return out
}

// egressStateOf 把一条 ExecutorHealth 投影为出口就绪快照(纯函数,便于单测)。
func egressStateOf(e protocol.ExecutorHealth) egressState {
	switch {
	case !e.Online && e.LastSeen != 0:
		return egressState{Text: "offline"}
	case e.Seg2.Unavailable != "":
		return egressState{Text: "notready"}
	default:
		return egressState{Ready: true, Text: "ready", Latency: compactLatency(e.Total)}
	}
}

// applyEgressStatus 把出口就绪落到隧道行上:表格 STATUS 追加出口健康,JSON 补 egress_status/egress_ready。
// 只写 statusDisplay,不改 Status —— JSON 的 status 必须保持纯状态值(running/stopped)。
// stopped 的隧道没有出口可探,保持原样;探测不可用(m 为 nil)或台账里没有该 egress 时记 unknown。
// 纯函数,便于单测。
func applyEgressStatus(r *psRow, m map[string]egressState) {
	if r.Type != "tunnel" || r.Status != "running" {
		return
	}
	st, ok := m[r.Egress]
	if !ok {
		r.EgressStatus = "unknown"
		r.statusDisplay = r.Status + " (egress unknown)"
		return
	}
	r.EgressStatus = st.Text
	ready := st.Ready
	r.EgressReady = &ready
	if st.Ready {
		if st.Latency != "" {
			r.statusDisplay = fmt.Sprintf("%s (egress ready %s)", r.Status, st.Latency)
		} else {
			r.statusDisplay = fmt.Sprintf("%s (egress ready)", r.Status)
		}
		return
	}
	r.statusDisplay = fmt.Sprintf("%s (egress %s)", r.Status, st.Text)
}

func runPs() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	targetNode := resolveNodeTarget(*psNode, *psExec)

	// 远端任务是「尽力而为」:本地 node 持有的隧道不经中转,后端建不出来(或中转暂不可达)
	// 也必须照常展示本地隧道,故这里只告警不退出。
	var tasks []backend.TaskInfo
	if b, berr := getBackendWithAdaptiveLoopback(cfg); berr != nil {
		fmt.Fprintf(os.Stderr, "Warning: create backend: %v\n", berr)
	} else if tb, ok := b.(backend.TaskSupervisorBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ts, lerr := tb.TaskList(ctx, targetNode, *psAll)
		cancel()
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "Warning: list remote tasks: %v\n", lerr)
		} else {
			tasks = ts
		}
	}

	// 本地隧道只在 node 在运行时才可能被持有;node 未运行即视为无隧道(不隐式拉起)。
	var tunnels []tunnelState
	if daemon.Get(daemon.PidFile("node")).Running {
		resp, terr := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpList})
		switch {
		case terr != nil:
			fmt.Fprintf(os.Stderr, "Warning: list local tunnels: %v\n", terr)
		case !resp.OK:
			fmt.Fprintf(os.Stderr, "Warning: list local tunnels: %s\n", resp.Error)
		default:
			tunnels = filterTunnels(resp.Tunnels, targetNode, *psAll)
		}
	}

	// 合并:任务行在前,隧道行在后。出口探测只在确有运行中的隧道时才发(一次中转查询,1.5s 上限),
	// 避免常态 `relay ps` 凭空多一次 RPC。
	var egress map[string]egressState
	for _, ts := range tunnels {
		if ts.State == "running" {
			egress = queryEgressStates(cfg)
			break
		}
	}
	rows := make([]psRow, 0, len(tasks)+len(tunnels))
	for _, t := range tasks {
		rows = append(rows, taskToPSRow(t))
	}
	for _, ts := range tunnels {
		r := tunnelToPSRow(ts)
		applyEgressStatus(&r, egress)
		rows = append(rows, r)
	}

	if *psJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return
	}

	if len(rows) == 0 {
		if *psAll {
			fmt.Println("No tasks found.")
		} else {
			fmt.Println("No running tasks found (use -a to show all).")
		}
		return
	}

	fmt.Printf("%-8s %-12s %-16s %-30s %-16s %-6s %-16s %s\n", "TYPE", "TASK ID", "NODE", "STATUS", "PORTS", "EXIT", "CREATED", "COMMAND")
	for _, r := range rows {
		fmt.Printf("%-8s %-12s %-16s %-30s %-16s %-6s %-16s %s\n", r.Type, r.ID, r.Node, r.displayStatus(), r.Ports, r.Exit, r.Created, r.Command)
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

	ctx := context.Background()
	targetNode := resolveNodeTarget(*logsNode, *logsExec)

	// 后端建不出来(中转不可达)时降级:隧道日志完全在本地、不经中转,应仍可查看。
	var tb backend.TaskSupervisorBackend
	if b, berr := getBackendWithAdaptiveLoopback(cfg); berr != nil {
		fmt.Fprintf(os.Stderr, "Warning: create backend: %v\n", berr)
	} else if x, ok := b.(backend.TaskSupervisorBackend); ok {
		tb = x
	} else {
		fmt.Fprintf(os.Stderr, "Warning: backend does not support task supervisor\n")
	}

	// 显式给了 -e/-w 且后端可用时按任务直接寻址(与 runStop 同构),不做任务名解析;
	// 只有在未指定目标时才在任务台账里反查节点,查不到即回退到本地隧道日志。
	knownTask := targetNode != "" && tb != nil
	if targetNode == "" && tb != nil {
		if tasks, err := tb.TaskList(ctx, "", true); err == nil {
			for _, t := range tasks {
				if t.ID == *logsTaskID {
					targetNode = t.Node
					knownTask = true
					break
				}
			}
		}
	}
	if !knownTask {
		// 先问本地 node 的隧道持有者:隧道是否存在以 holder 为准,而不是日志里有没有匹配行。
		// 新开且健康的隧道不会写任何 `[tunnel <egress>]` 前缀行,用「零命中」反推「不是隧道」
		// 会把 `relay logs <egress> -f` 这个最常用的场景判死。
		if tunnelEgressKnown(*logsTaskID) {
			showTunnelLogs(*logsTaskID, *logsFollow, *logsTail)
			return
		}
		fmt.Fprintf(os.Stderr, "logs failed: no task or tunnel named %s\n", *logsTaskID)
		os.Exit(1)
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

	// 后端建不出来(中转不可达)时降级:本地隧道不经中转,必须仍能停掉并释放监听端口,
	// 故这里只告警不退出,与 runPs/runLogs 同一姿态。远端任务停不掉只能如实报错。
	var tb backend.TaskSupervisorBackend
	if b, berr := getBackendWithAdaptiveLoopback(cfg); berr != nil {
		fmt.Fprintf(os.Stderr, "Warning: create backend: %v\n", berr)
	} else if x, ok := b.(backend.TaskSupervisorBackend); ok {
		tb = x
	} else {
		fmt.Fprintf(os.Stderr, "Warning: backend does not support task supervisor\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetNode := resolveNodeTarget(*stopNode, *stopExec)
	if targetNode == "" && tb != nil {
		if tasks, err := tb.TaskList(ctx, "", true); err == nil {
			for _, t := range tasks {
				if t.ID == *stopTaskID {
					targetNode = t.Node
					break
				}
			}
		}
	}
	// ID 不是已知任务(或后端不可用):退回本地 node 持有的隧道(egress 身份,如 `relay stop eg-node`)。
	if targetNode == "" || tb == nil {
		if stopLocalTunnel(*stopTaskID) {
			return
		}
		if tb == nil {
			fmt.Fprintf(os.Stderr, "stop failed: backend unavailable and no local tunnel named %s\n", *stopTaskID)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "stop failed: no task or tunnel named %s\n", *stopTaskID)
		os.Exit(1)
	}
	if err := tb.TaskStop(ctx, targetNode, *stopTaskID); err != nil {
		// 兜底:ID 也可能恰是一条隧道 egress。任务停不掉时再试一次本地隧道,能关掉就不算失败。
		if stopLocalTunnel(*stopTaskID) {
			return
		}
		fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Stopped task %s\n", *stopTaskID)

	// 联动撤销本地 node 持有的对应端口转发(若存在):远端任务已停,释放本机监听。
	// 仅当本机 node daemon 在运行(其 holder 才持有本地转发)时尝试 unregister;node 未运行
	// (含 kill -9 残留的陈旧 socket)时该任务本就没有本地转发,静默跳过避免误导性告警。
	if d := daemon.Get(daemon.PidFile("node")); d.Running {
		if err := sendForwardUnregister(forwardSockPath(), *stopTaskID); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: failed to stop local port-forward for task %s: %v\n", *stopTaskID, err)
		}
	}
}

// stopLocalTunnel 关闭本地 node 持有的一条隧道(供 relay stop 复用)。node 未运行、socket
// 不可达或 holder 未命中该 egress 时均返回 false(调用方据此回落到原有的报错路径)。
func stopLocalTunnel(egress string) bool {
	if egress == "" {
		return false
	}
	if !daemon.Get(daemon.PidFile("node")).Running {
		return false
	}
	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpClose, Egress: egress})
	if err != nil || !resp.OK {
		return false
	}
	fmt.Printf("Stopped tunnel %s\n", egress)
	return true
}

// statusTimeout 是 `relay status` 单条命令的上限(含对中转 status 请求的等待)。
const statusTimeout = 15 * time.Second

// PeerNodeItem 描述拓扑中的一个对等节点
type PeerNodeItem struct {
	ID       string   `json:"id"`
	Role     string   `json:"role"`
	Status   string   `json:"status"`
	Platform string   `json:"platform"`
	CPU      string   `json:"cpu"`
	Memory   string   `json:"memory"`
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

	// 清理离线节点:把中转台账里「已知但离线」的 executor 移除,并报告清理数。
	if *nodesPrune {
		n, perr := rb.PruneOfflineExecutors(ctx)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "Prune offline executors error: %v\n", perr)
			os.Exit(1)
		}
		fmt.Printf("Pruned %d offline executor(s)\n", n)
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
	localCPU, localMem := formatNodeResource(localRes, withUsage)

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
	transitCPU, transitMem := formatNodeResource(transitSysRes, withUsage)

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

	// 3. 远端 Agent / Executor 节点。本机 node 若以 agent 运行会作为 executor 注册进台账,
	// 而它已在「1. 本机节点」以 (local) 行呈现,这里按本机 backend.config.executor_id 去重合并。
	localExecID, _ := cfg.Backend.Config["executor_id"].(string)
	for _, e := range st.Executors {
		if e.ExecutorID == "hub" {
			// hub 自身作为 agent 已合并在 hub (transit) 呈现，避免重复行
			continue
		}
		if localExecID != "" && e.ExecutorID == localExecID {
			// 本机 node 已作为 (local) 行呈现,避免同一台本机出现两行
			continue
		}
		// 离线判定以响应中的 LastSeen 为准:旧版中转不返回 Online/LastSeen(Online 缺省为 false),
		// 仅靠 !Online 会把旧中转的所有在线节点误标为 Offline。故仅在显式携带 LastSeen(>0)时视为离线。
		offline := !e.Online && e.LastSeen != 0
		stText := "Ready"
		switch {
		case offline:
			stText = "Offline"
		case e.Seg2.Unavailable != "":
			stText = "NotReady"
		}
		latency := segmentText(e.Total)
		if offline {
			latency = "-"
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
		execCPU, execMem := formatNodeResource(execSysRes, withUsage)

		nodes = append(nodes, PeerNodeItem{
			ID:       e.ExecutorID,
			Role:     "agent",
			Status:   stText,
			Platform: platform,
			CPU:      execCPU,
			Memory:   execMem,
			Version:  identStr(e.Version),
			Latency:  latency,
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

func formatNodeResource(res sysinfo.ResourceInfo, withUsage bool) (cpu, mem string) {
	if withUsage {
		return res.FormatCPUUsage(), res.FormatMemUsage()
	}
	return res.FormatCPUSummary(), res.FormatMemSummary()
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
