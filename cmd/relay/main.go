package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/daemon"
	"github.com/user/relay/internal/exchange"
	"github.com/user/relay/internal/jobrunner"
	"github.com/user/relay/internal/logx"
	"github.com/user/relay/internal/portfwd"
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
	// 任务的写入/校验由任务模式实现负责)。缺省留空以便区分「未给出」与「显式 --restart=no」,
	// 任务模式据此要求 -d(R5);取值校验抽成 restartPolicyValue 纯函数共用。
	execRestartPolicy = execCmd.Flag("restart", "Restart policy at the owning daemon's next start: always|no (task: executor start; tunnel: local node start)").String()
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
	psJSON = psCmd.Flag("json", "Output as JSON (full values, no column truncation)").Bool()

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

	// start/restart commands - 根级生命周期动词(R1):按名字操作已存在的任务或隧道,从不创建对象。
	// 任务寻址沿用 stop/logs 的 -e/--node;隧道寻址就用名字本身(egress),不需要 -e。
	startCmd    = kingpin.Command("start", "Start an existing task or node-held tunnel by name (never creates one)")
	startTarget = startCmd.Arg("name", "Task ID or tunnel egress to start").Required().String()
	startExec   = startCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	startNode   = startCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

	restartCmd    = kingpin.Command("restart", "Restart an existing task or node-held tunnel by name")
	restartTarget = restartCmd.Arg("name", "Task ID or tunnel egress to restart").Required().String()
	restartExec   = restartCmd.Flag("executor", "Target executor node").Short('e').HintAction(getNodeHints).String()
	restartNode   = restartCmd.Flag("node", "Target node ID (alias for --executor)").HintAction(getNodeHints).String()

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

	// 隧道没有独立命名空间:创建走 `relay run -t|--type=tunnel`(R14/KD6),生命周期走根级
	// `start`/`restart`/`stop`/`logs`(R1),读取统一走 `relay ps`(TYPE tunnel 行)。

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
	case selectedCmd == startCmd.FullCommand():
		runStart()
	case selectedCmd == restartCmd.FullCommand():
		runRestart()
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

	// 执行方进程启动路径:显式构造 supervisor 并重放策略为 always 的任务一次(KTD4)。
	// 每进程只触发一次;单条失败不阻塞启动,原因写入该任务自己的日志。
	relaybackend.ReplayAlwaysTasks()

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

// resolveTaskRestartPolicy 校验任务模式的 --restart/--no-restart(R5):取值复用与隧道共用的
// restartPolicyValue 纯函数;任一旗标显式给出时都要求 -d/--detach —— 前台执行没有可承载策略
// 的受管任务,带它时直接报错,不静默忽略。纯函数,不读全局旗标,便于单测与隧道命名空间退役后
// 独立使用。
func resolveTaskRestartPolicy(policy string, noRestart, detach bool) (string, error) {
	resolved, err := restartPolicyValue(policy, noRestart)
	if err != nil {
		return "", err
	}
	explicit := strings.TrimSpace(policy) != "" || noRestart
	if explicit && !detach {
		return "", fmt.Errorf("--restart/--no-restart requires -d/--detach (a foreground run has no managed task to carry a restart policy)")
	}
	return resolved, nil
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
	var taskRestart string
	switch mode := strings.ToLower(strings.TrimSpace(*execType)); mode {
	case "", "task":
		if err := requireTaskCommand(*execCmdStr); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		policy, err := resolveTaskRestartPolicy(*execRestartPolicy, *execNoRestart, *execDetach)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		taskRestart = policy
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
		// 端口规格以解析后的结构上行(KTD9):记录必须无损带走绑定面,不能退化成 String() 的展示形态。
		var execPorts []portfwd.Spec
		for _, spec := range pfSpecs {
			execPorts = append(execPorts, *spec)
		}
		exit, err := eb.ExecStreamWithPorts(ctx, targetExecutor, cmd, execCwd, 0, gitSyncPayload, *execDetach, execPorts, taskRestart, func(chunk backend.ExecChunk) {
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
			// 非前台(经 holder)的注册路径由 CLI 自己复述非 loopback 告警:holder 那条只落到
			// node.log,用户看不到(见 warnNonLoopbackForwards)。
			warnNonLoopbackForwards(pfSpecs, registered)
			// 转发行里已含 task id,故这条分支不再重复 echo(见下面的 else if)。
			reportForwardRegistration(registered, rejected, detachTaskID)
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
// 行是 JSON 的来源,故一律存完整值;列宽裁剪只发生在 psLine 的表格渲染里(--json 必须无损)。
type psRow struct {
	Type    string `json:"type"` // "task" | "tunnel"
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"`
	Status  string `json:"status"`
	Ports   string `json:"ports,omitempty"`   // task: comma-joined; tunnel: the listen address
	Exit    string `json:"exit,omitempty"`    // task: exit code or "-"
	Created string `json:"created,omitempty"` // 相对时间("3m ago")
	Command string `json:"command"`           // task: 完整命令行; tunnel: "egress <egress>"
	Egress  string `json:"egress,omitempty"`  // tunnel only
	Restart string `json:"restart,omitempty"` // 任务与隧道共用(R20:任务行显示 restart 策略;隧道行早就用它)
	// 隧道出口就绪(取自中转台账,与 `relay nodes` 同一事实来源)。出口是 SOCKS5 的三段链路末段,
	// 本地 listener 活着不代表出口活着,故单独成列/字段。
	EgressStatus string `json:"egress_status,omitempty"` // ready | offline | notready | unknown
	EgressReady  *bool  `json:"egress_ready,omitempty"`  // nil = 未探测或未知

	// statusDisplay 仅供表格:STATUS + 出口就绪。JSON 的 status 保持纯状态值(running/stopped),
	// 免得机器消费者去解析人类文案。
	statusDisplay string
}

// psRowFormat 是 relay ps 表格的行格式。独立成常量以便表头与行共用同一列宽(R20 的 RESTART 列
// 必须与每一行对齐;表头由 psHeader 渲染,行由 psLine 渲染)。
const psRowFormat = "%-8s %-12s %-16s %-30s %-16s %-8s %-6s %-16s %s\n"

// PORTS 的定宽(与 psRowFormat 里那一段一致):它后面还有列,超宽会把整张表挤歪,只能裁。
// COMMAND 是末列,裁它不影响对齐、只会丢信息,故不裁。
const psPortWidth = 16

// relay ps 的出口探测预算:探针预算必须明显短于本地等待上限,否则慢出口会让本地先超时,
// 隧道行只能退化成语义模糊的 `unknown`,而不是「出口 notready」。
const (
	psEgressProbeTimeout = 800 * time.Millisecond
	psEgressQueryTimeout = 3 * time.Second
)

// psHeader 渲染 relay ps 的表头行。独立成函数以便单测钉住 R20 的 RESTART 列位置。
func psHeader() string {
	return fmt.Sprintf(psRowFormat, "TYPE", "TASK ID", "NODE", "STATUS", "PORTS", "RESTART", "EXIT", "CREATED", "COMMAND")
}

// psLine 渲染表格里的一行。列宽裁剪只在这里发生:psRow 是 --json 的来源,必须保持完整值。
func psLine(r psRow) string {
	return fmt.Sprintf(psRowFormat, r.Type, r.ID, r.Node, r.displayStatus(), fitCell(r.Ports, psPortWidth), r.Restart, r.Exit, r.Created, r.Command)
}

// fitCell 把单元格收到定宽列的宽度,超出部分以 "..." 结尾。按 rune 裁,避免切断多字节字符
// (主机名可以是 Unicode,按字节裁会切出非法 UTF-8)。
func fitCell(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width-3]) + "..."
}

// displayStatus 返回表格用的 STATUS 单元格:叠加了出口就绪时用叠加文案,否则用纯 status。
func (r psRow) displayStatus() string {
	if r.statusDisplay != "" {
		return r.statusDisplay
	}
	return r.Status
}

// portDisplay 渲染一条端口转发规格用于 ps 的 PORTS 列。历史形态(回环绑定)保持字节一致,
// 非回环时前缀绑定主机(如 "0.0.0.0:8080"),让发布面在展示上也可见(R21/KTD9)。
func portDisplay(s portfwd.Spec) string {
	if isLoopbackAddr(s.BindHost) {
		return s.String()
	}
	return s.BindHost + ":" + s.String()
}

// taskToPSRow 把一个远端 detach 任务投影为统一行(纯函数,便于单测)。只做兜底,不做裁剪:
// Node 空 → (unknown),其余字段原样保留完整值。
func taskToPSRow(t backend.TaskInfo) psRow {
	nodeDisp := t.Node
	if nodeDisp == "" {
		nodeDisp = "(unknown)"
	}
	// 从结构化记录渲染:回环绑定显示与历史一致的端口形态,非回环时补上绑定面,
	// 使 0.0.0.0 这类发布面在 ps 里可见(R21/KTD9)。portDisplay 从不返回空串,故无端口时
	// 由 orDash 落到占位符。
	parts := make([]string, len(t.Ports))
	for i, p := range t.Ports {
		parts[i] = portDisplay(p)
	}
	exitDisp := "-"
	if t.Status != taskStatusRunning {
		exitDisp = fmt.Sprintf("%d", t.ExitCode)
	}
	// CREATED 取首次创建时间而非最近一次启动(R12):重启不重置 CREATED。老执行方上行 CreatedAt=0
	// 时回退到最近一次启动时间(与既有展示一致)。
	createdAt := t.StartTime
	if t.CreatedAt != 0 {
		createdAt = t.CreatedAt
	}
	return psRow{
		Type:    "task",
		ID:      t.ID,
		Node:    nodeDisp,
		Status:  t.Status,
		Ports:   orDash(strings.Join(parts, ",")),
		Exit:    exitDisp,
		Created: formatRelativeTime(time.Unix(createdAt, 0)),
		Command: t.Cmd,
		Restart: t.Restart,
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

// queryEgressStates 向中转查询指定出口 executor 的在线就绪快照。判定与 `relay nodes` 完全一致
// (离线看 LastSeen;Seg2 不可用=NotReady)。只探 egressIDs 里的出口,且探针预算短于本地等待上限,
// 所以慢出口只会得到一个及时的 notready,不会把整条 `relay ps` 拖到超时;任何错误静默降级为
// nil——调用方据此把隧道行标成 unknown,而不是让 `relay ps` 因为探测失败而失败。
func queryEgressStates(cfg *config.Config, egressIDs []string) map[string]egressState {
	rb, err := getRelayBackend(cfg)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), psEgressQueryTimeout)
	defer cancel()
	st, err := rb.StatusTargeted(ctx, false, egressIDs, psEgressProbeTimeout)
	if err != nil || !st.OK {
		return nil
	}
	out := make(map[string]egressState, len(st.Executors))
	for _, e := range st.Executors {
		out[e.ExecutorID] = egressStateOf(e)
	}
	return out
}

// runningEgressIDs 收集确有运行中隧道的出口 executor_id(去重、保序)。没有运行中的隧道时返回空,
// 调用方据此完全跳过中转探测。
func runningEgressIDs(tunnels []tunnelState) []string {
	var ids []string
	seen := make(map[string]bool, len(tunnels))
	for _, ts := range tunnels {
		if ts.State != "running" || ts.Egress == "" || seen[ts.Egress] {
			continue
		}
		seen[ts.Egress] = true
		ids = append(ids, ts.Egress)
	}
	return ids
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

	// 本地隧道:node 在运行时问 holder(内存态是权威),node 未运行时降级读意图文件并把 state
	// 一律标成 stopped(R19)—— 没有任何进程持有监听,文件里残留的 running 已不再成立。
	// 无 -a 时 stopped 隧道照旧不出现在表里(filterTunnels 语义不变),所以 node 未运行且没给 -a
	// 的时候连文件都不读:读出来的东西注定被丢弃。
	var tunnels []tunnelState
	nodeRunning := nodeDaemonRunning()
	if nodeRunning || *psAll {
		var terr error
		tunnels, terr = tunnelStatesForRead(tunnelStatePath(), tunnelSockPath(), nodeRunning)
		if terr != nil {
			fmt.Fprintf(os.Stderr, "Warning: list local tunnels: %v\n", terr)
		}
		tunnels = filterTunnels(tunnels, targetNode, *psAll)
	}

	// 合并:任务行在前,隧道行在后。出口探测只针对确有运行中隧道的出口(一次中转查询),没有就不发,
	// 避免常态 `relay ps` 凭空多一次 RPC。
	var egress map[string]egressState
	if ids := runningEgressIDs(tunnels); len(ids) > 0 {
		egress = queryEgressStates(cfg, ids)
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

	fmt.Print(psHeader())
	for _, r := range rows {
		fmt.Print(psLine(r))
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

	tb := taskSupervisorBackend(cfg)
	targetNode := resolveNodeTarget(*logsNode, *logsExec)
	nodeRunning := nodeDaemonRunning()

	// 名字解析(KTD8):任务清单(含已停止,all=true)优先,再本机 holder,最后意图文件。后端建不出来
	// (中转不可达)时降级:隧道日志完全在本地、不经中转,应仍可查看。
	listCtx, cancelList := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelList()
	res := resolveLifecycle(*logsTaskID, lifecycleDepsFor(listCtx, tb, targetNode, nodeRunning))
	switch res.Kind {
	case lifecycleKindTask:
		err := tb.TaskLogs(context.Background(), res.Node, res.Task.ID, *logsFollow, *logsTail, func(chunk backend.ExecChunk) {
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
	case lifecycleKindTunnel:
		// R19:意图文件确认它存在,但没有持有者在写日志 —— 如实说「node 未运行」,而不是「没有这个
		// 任务或隧道」。node 在运行时走既有日志呈现(零匹配行 = 尚无输出,见 showTunnelLogs)。
		if !res.NodeRunning {
			fmt.Fprintf(os.Stderr, "logs failed: local node is not running; %s is a persisted tunnel intent (start the node with `relay node start` to see its log)\n", *logsTaskID)
			os.Exit(1)
		}
		showTunnelLogs(*logsTaskID, *logsFollow, *logsTail)
	default:
		fmt.Fprintf(os.Stderr, "logs failed: %v\n", lifecycleUnresolvedError(*logsTaskID, res))
		os.Exit(1)
	}
}

func runStop() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	tb := taskSupervisorBackend(cfg)
	targetNode := resolveNodeTarget(*stopNode, *stopExec)
	nodeRunning := nodeDaemonRunning()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 名字解析(KTD8):任务清单(含已停止)优先,再本机 holder,最后意图文件。后端建不出来
	// (中转不可达)时降级:本地隧道不经中转,必须仍能停掉并释放监听端口。
	res := resolveLifecycle(*stopTaskID, lifecycleDepsFor(ctx, tb, targetNode, nodeRunning))
	switch res.Kind {
	case lifecycleKindTask:
		if err := tb.TaskStop(ctx, res.Node, res.Task.ID); err != nil {
			// 兜底:ID 也可能恰是一条隧道 egress。任务停不掉时再试一次本地隧道,能关掉就不算失败。
			if stopSucceeded(stopLocalTunnel(tunnelStatePath(), tunnelSockPath(), *stopTaskID, nodeRunning)) {
				return
			}
			fmt.Fprintf(os.Stderr, "stop failed: task %s on executor %s: %v\n", res.Task.ID, res.Node, err)
			os.Exit(1)
		}
		fmt.Printf("Stopped task %s\n", res.Task.ID)

		// 联动撤销本地 node 持有的对应端口转发(若存在):远端任务已停,释放本机监听。
		// 仅当本机 node daemon 在运行(其 holder 才持有本地转发)时尝试 unregister;node 未运行
		// (含 kill -9 残留的陈旧 socket)时该任务本就没有本地转发,静默跳过避免误导性告警。
		if nodeRunning {
			if err := sendForwardUnregister(forwardSockPath(), res.Task.ID); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: failed to stop local port-forward for task %s: %v\n", res.Task.ID, err)
			}
		}
	case lifecycleKindTunnel:
		// R2:已停止的隧道再停一次不算失败,但名字得还在(holder 或意图文件里);意图移除照常生效(R18)。
		// 「关不掉」(holder 不可达/拒绝)与「名字不在」必须分开答复:名字上一刻才刚解析出来,
		// 把传输失败说成「没有这个对象」会把操作者推去重建,而旧监听还占着端口(R21 隧道侧)。
		out, err := stopLocalTunnel(tunnelStatePath(), tunnelSockPath(), res.State.Egress, nodeRunning)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
			os.Exit(1)
		}
		if !stopSucceeded(out, nil) {
			fmt.Fprintf(os.Stderr, "stop failed: no task or tunnel named %s\n", *stopTaskID)
			os.Exit(1)
		}
	default:
		// 名字解析不到对象时,仍可能有一个旧版 per-egress `tunnel-<egress>` daemon 占着监听地址
		// (创建失败时打印的提示正是让人来跑这条命令,见 tunnelLegacyDaemonHint)。这里就地清掉它,
		// 让那条提示成真;确有进程被停时就不再报「没有这个对象」—— 那会与刚完成的动作自相矛盾(#8)。
		if stopLegacyTunnelDaemon(*stopTaskID) {
			fmt.Printf("stopped legacy tunnel daemon %s (no task or node-held tunnel has that name)\n", *stopTaskID)
			return
		}
		fmt.Fprintf(os.Stderr, "stop failed: %v\n", lifecycleUnresolvedError(*stopTaskID, res))
		os.Exit(1)
	}
}

// nodeDaemonRunning 是「本机 node daemon 是否在运行」在 CLI 侧的判据:node.pid 说在跑,或
// 隧道控制 socket 能拨通(同一个探针,ensureLocalRelayNode 也以它作为就绪信号)。只认 pid 文件会
// 把前台 `relay node run` 判成「没在跑」—— 它不写 node.pid,但 holder 确实在监听,于是 ps/stop/
// restart 全去读改过期的意图文件。抽成包级变量供单测注入:node 未运行的分支在真机上无法安全构造
// (会去拨真实的 ~/.relay/tunnel.sock / 改真实的 ~/.relay/tunnels.json),测试须临时替换它并还原
// —— 与 relaybackend.SetExecutorRole 同一姿态。socket 不存在时 unix 拨号立即失败,不开销。
// ensureLocalRelayNode 另有自己的 daemon.Get 调用:它需要拿到具体的错误好把「起不来」的原因
// 讲清楚,不只是一个是/否。
var nodeDaemonRunning = func() bool {
	if daemon.Get(daemon.PidFile("node")).Running {
		return true
	}
	conn, err := net.DialTimeout("unix", tunnelSockPath(), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// taskSupervisorBackend 尽力而为地构造任务后端(TaskSupervisorBackend):建不出来(中转不可达)
// 或后端不支持时只告警不退出,调用方自行降级 —— 本地隧道不经中转,列出/停止/看日志必须照常可用。
func taskSupervisorBackend(cfg *config.Config) backend.TaskSupervisorBackend {
	var tb backend.TaskSupervisorBackend
	if b, berr := getBackendWithAdaptiveLoopback(cfg); berr != nil {
		fmt.Fprintf(os.Stderr, "Warning: create backend: %v\n", berr)
	} else if x, ok := b.(backend.TaskSupervisorBackend); ok {
		tb = x
	} else {
		fmt.Fprintf(os.Stderr, "Warning: backend does not support task supervisor\n")
	}
	return tb
}

// lifecycleKind 是名字解析出的对象种类。
type lifecycleKind int

const (
	lifecycleKindUnknown lifecycleKind = iota
	lifecycleKindTask
	lifecycleKindTunnel
)

// lifecycleTarget 是一次名字解析的结果。判别式的 struct 而非裸字符串:调用方需要据此给出三种
// 不同的答复 —— 名字不存在(R1 的创建提示)、任务在不可达的执行方上(R4 的「可能离线、用 -e」)、
// 隧道而本机 node 未运行(R4/R19 的降级说明,不静默)。
type lifecycleTarget struct {
	Kind lifecycleKind

	// 任务侧(Kind == lifecycleKindTask):Node 是执行方 executor_id(隧道侧不设,身份取 State.Egress),
	// Task 是任务记录(带 Ports/Status/Restart/CreatedAt,R13/R20 据此补登记与呈现)。
	Node string
	Task backend.TaskInfo

	// 隧道侧(Kind == lifecycleKindTunnel):身份取自 State.Egress(Node 只承载任务侧的执行方),
	// State 是 holder 或意图文件里的现状,NodeRunning 说明本机 node 当时是否在运行
	// (未运行时 State 已被降级标成 stopped)。
	State       tunnelState
	NodeRunning bool

	// Target 是用户用 -e/--node 显式指明的执行方(空 = 未指明,靠清单反查)。
	Target string
	// Unreachable 非空表示「查不到」不是「不存在」,而是清单/现状根本问不到(执行方离线或中转
	// 不可达)。此时按 R4 报「该执行方可能离线、可用 -e 指定」,不提示重建。
	Unreachable string
}

// lifecycleDeps 是名字解析的外部依赖(任务清单与本机隧道现状),抽成函数值以便单测注入,
// 不必凭空构造一个在线 node 或真中转。
type lifecycleDeps struct {
	Target      string
	NodeRunning bool
	ListTasks   func() ([]backend.TaskInfo, error)
	Tunnels     func() ([]tunnelState, error)
}

// taskBackendUnavailable 是「任务后端问不到」的唯一措辞:中转不可达,或后端不支持任务监管。
// 名字解析与各调用点共用同一句,同一种情形不再有好几种拼法。
const taskBackendUnavailable = "task backend unavailable (transit unreachable?)"

// lifecycleDepsFor 构造生命周期动词共用的名字解析依赖(KTD8):任务清单(含已停止,all=true)优先,
// 本机 holder 与意图文件兜底。ctx 由调用方给,各条命令各自持有自己的时限,不互相延长。
func lifecycleDepsFor(ctx context.Context, tb backend.TaskSupervisorBackend, target string, nodeRunning bool) lifecycleDeps {
	return lifecycleDeps{
		Target:      target,
		NodeRunning: nodeRunning,
		ListTasks: func() ([]backend.TaskInfo, error) {
			if tb == nil {
				return nil, errors.New(taskBackendUnavailable)
			}
			return tb.TaskList(ctx, target, true)
		},
		Tunnels: func() ([]tunnelState, error) {
			return tunnelStatesForRead(tunnelStatePath(), tunnelSockPath(), nodeRunning)
		},
	}
}

// resolveLifecycle 按对象名解析(KTD8/R3):先任务清单(含已停止的,all=true),再本机 holder,
// 最后意图文件;同名同时命中任务与隧道时任务优先,沿用 runLogs/runStop 的既有顺序。三种
// 「查不到」被区分开(R1/R4),调用方各按其措辞答复。
func resolveLifecycle(name string, deps lifecycleDeps) lifecycleTarget {
	out := lifecycleTarget{NodeRunning: deps.NodeRunning, Target: deps.Target}
	if name == "" {
		return out
	}
	if deps.ListTasks == nil {
		out.Unreachable = taskBackendUnavailable
	} else if tasks, err := deps.ListTasks(); err != nil {
		out.Unreachable = err.Error()
	} else {
		for _, t := range tasks {
			if t.ID == name {
				node := t.Node
				if node == "" {
					node = deps.Target
				}
				return lifecycleTarget{Kind: lifecycleKindTask, Node: node, Task: t, Target: deps.Target, NodeRunning: deps.NodeRunning}
			}
		}
	}
	// 第二、三级:本机隧道 —— node 在运行时问 holder,未运行时读意图文件。清单都问不到
	// (Unreachable)时仍继续查隧道:本地隧道不经中转,停/看日志必须照常可用(R4 的隧道侧)。
	if deps.Tunnels != nil {
		states, err := deps.Tunnels()
		if err == nil {
			for _, st := range states {
				if st.Egress == name {
					return lifecycleTarget{Kind: lifecycleKindTunnel, State: st, Target: deps.Target, NodeRunning: deps.NodeRunning}
				}
			}
		} else if out.Unreachable == "" {
			out.Unreachable = err.Error()
		}
	}
	return out
}

// lifecycleCreateHint 是可创建对象的两种方式,任务与隧道并列给出(R1)。
const lifecycleCreateHint = "`relay run <cmd> -d` (task) or `relay run -t tunnel -p <[host:]port> -e <egress>` (tunnel)"

// lifecycleUnresolvedError 生成「名字解析不到对象」的说明文案(R1/R4)。返回的文案不含动词前缀,
// 由调用方补「start failed: 」这类头。四种情形分别答复:
//   - 用户用 -e/--node 限定了执行方且它答了话 → 确定的「这个执行方上没有这个名字」,带 id(R4);
//   - 用户限定了执行方但问不到 → 带 id + 可能离线(R4),同样不提示重建;
//   - 清单/现状问不到 → 执行方可能离线、可用 -e 指定,不提示重建(R4);
//   - 都能问但名字不在其中 → 不把「查不到」说成「不存在」(R4),并给出创建方式(R1)。
func lifecycleUnresolvedError(name string, t lifecycleTarget) string {
	switch {
	case t.Target != "" && t.Unreachable == "":
		return fmt.Sprintf("no task or tunnel named %q (task lookup was limited to executor %q); to create one: %s", name, t.Target, lifecycleCreateHint)
	case t.Target != "":
		return fmt.Sprintf("cannot look up %q on executor %q: %s (that executor may be offline)", name, t.Target, t.Unreachable)
	case t.Unreachable != "":
		return fmt.Sprintf("cannot look up %q: %s (the executor may be offline; pass -e/--executor <executor_id> to address it directly)", name, t.Unreachable)
	default:
		return fmt.Sprintf("no task or tunnel named %q found (an offline executor's tasks cannot be listed this way; use -e/--executor <executor_id> to address one directly); to create one: %s", name, lifecycleCreateHint)
	}
}

// lifecycleVerb 是根级两个「让对象跑起来」的动词:start 对已在运行的任务不重启(R2),
// restart 总是先停掉再重开(R9/R11)。
type lifecycleVerb int

const (
	verbStart lifecycleVerb = iota
	verbRestart
)

func (v lifecycleVerb) String() string {
	if v == verbStart {
		return "start"
	}
	return "restart"
}

func (v lifecycleVerb) pastTense() string {
	if v == verbStart {
		return "Started"
	}
	return "Restarted"
}

// taskStatusRunning 是任务记录里「运行中」的状态值(supervisor 的 TaskStatusRunning)。
const taskStatusRunning = "running"

// runStart 是根级 `relay start`(R1/R2/R13):让一个已存在的任务或隧道恢复运行。
func runStart() { runLifecycleVerb(verbStart) }

// runRestart 是根级 `relay restart`(R9):让一个已存在的任务或隧道按记录重开。
func runRestart() { runLifecycleVerb(verbRestart) }

// runLifecycleVerb 是 start/restart 两个根级动词的共享骨架:按名字解析(KTD8) → 分派到任务侧
// (经中转下发 U1 的 MsgTaskRestart)或隧道侧(经本机 holder 控制消息),再按 R13 幂等补登记
// 本机转发规则。任何一步失败都以非零码退出(R4 任务侧;隧道侧在 node 未运行时降级说明)。
func runLifecycleVerb(verb lifecycleVerb) {
	name, execVal, nodeVal := *startTarget, *startExec, *startNode
	if verb == verbRestart {
		name, execVal, nodeVal = *restartTarget, *restartExec, *restartNode
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	tb := taskSupervisorBackend(cfg)
	target := resolveNodeTarget(nodeVal, execVal)
	nodeRunning := nodeDaemonRunning()

	listCtx, cancelList := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelList()
	res := resolveLifecycle(name, lifecycleDepsFor(listCtx, tb, target, nodeRunning))
	switch res.Kind {
	case lifecycleKindTask:
		if err := runTaskLifecycle(verb, cfg, tb, res); err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n", verb, err)
			os.Exit(1)
		}
	case lifecycleKindTunnel:
		if err := runTunnelLifecycle(verb, cfg, res); err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n", verb, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", verb, lifecycleUnresolvedError(name, res))
		os.Exit(1)
	}
}

// applyTaskLifecycle 是任务动词的动作部分(纯决策 + 一次协议调用,便于单测断言 R2 的「不重启」):
// start 作用于已在运行的任务时不重启、不报错,但仍返回端口规格供调用方按 R13 补登记转发;
// 否则下发同 id 重启(U1 的 MsgTaskRestart,R9)。返回是否真的重新下发。
func applyTaskLifecycle(verb lifecycleVerb, tb backend.TaskSupervisorBackend, node string, task backend.TaskInfo) ([]portfwd.Spec, bool, error) {
	if verb == verbStart && task.Status == taskStatusRunning {
		return task.Ports, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tb.TaskRestart(ctx, node, task.ID); err != nil {
		return nil, false, fmt.Errorf("task %s on executor %s: %w", task.ID, node, err)
	}
	return task.Ports, true, nil
}

// runTaskLifecycle 对解析出的任务记录执行 start/restart 并补登记本机转发(R13)。tb 在解析到任务
// 时非 nil(清单查询成功过),这里仍防御性判空以免未来调用点失守。
func runTaskLifecycle(verb lifecycleVerb, cfg *config.Config, tb backend.TaskSupervisorBackend, res lifecycleTarget) error {
	if tb == nil {
		return errors.New(taskBackendUnavailable)
	}
	ports, restarted, err := applyTaskLifecycle(verb, tb, res.Node, res.Task)
	if err != nil {
		return err
	}
	if !restarted {
		// R2:已在运行的对象不重启、不报错 —— 但仍按 R13 补登记本机转发规则(本机 node 重启后
		// 恢复端口的唯一路径),故这里绝不提前返回。
		fmt.Printf("task %s is already running (node %s)\n", res.Task.ID, res.Node)
	} else {
		fmt.Printf("%s task %s (node %s)\n", verb.pastTense(), res.Task.ID, res.Node)
	}
	// R13/R21:按任务记录里的完整端口规格(含绑定主机)幂等重登记本机转发 —— stop 之后再 start、
	// 或本机 node 重启之后再 start,已发布的本地端口都按原绑定面恢复可用。
	return reRegisterTaskForwards(cfg, forwardSockPath(), res.Task.ID, res.Node, ports, res.NodeRunning)
}

// reRegisterTaskForwards 按任务记录里的端口规格(含绑定主机)幂等重登记本机转发规则(R13/KTD5):
// 先确保本地 node(转发持有者)在线,再经 forward.sock 下发 register。措辞与非零退出姿态与
// detach 创建路径完全一致(见 sendForwardRegister 的调用处)。空规格直接返回。
// nodeRunning 由调用方传入:已知本机 node 在运行时不必再拨一次控制 socket 探活,register 自己
// 就会连上去 —— 那是同一个注册动作的第二次连接。
func reRegisterTaskForwards(cfg *config.Config, sockPath, taskID, node string, specs []portfwd.Spec, nodeRunning bool) error {
	if len(specs) == 0 {
		return nil
	}
	if !nodeRunning {
		if err := ensureLocalRelayNode(cfg, sockPath); err != nil {
			return fmt.Errorf("port-forward: %w", err)
		}
	}
	ptrs := make([]*PortForwardSpec, 0, len(specs))
	for i := range specs {
		ptrs = append(ptrs, &specs[i])
	}
	registered, rejected, err := sendForwardRegister(sockPath, taskID, node, ptrs)
	if err != nil {
		return fmt.Errorf("port-forward register error: %w", err)
	}
	warnNonLoopbackForwards(ptrs, registered)
	reportForwardRegistration(registered, rejected, taskID)
	return nil
}

// reportForwardRegistration 回显一次转发登记的落地情况:仍然在听的地址打到 stdout,被拒绝的
// 打到 stderr。detach 创建路径与 R13 的补登记路径共用同一份措辞。
func reportForwardRegistration(registered, rejected []string, taskID string) {
	for _, a := range registered {
		fmt.Printf("➜ [port-forward] %s still listening (task %s)\n", a, taskID)
	}
	for _, r := range rejected {
		fmt.Fprintf(os.Stderr, "WARNING: %s (task %s)\n", r, taskID)
	}
}

// warnNonLoopbackForwards 在调用方的 stderr 上逐条打印非 loopback 绑定的告警。holder 侧也记一条
// 到 node.log,但那是 node 的 stderr(daemon 已重定向),发起者本人看不到,故非前台(经 holder 注册)
// 的路径必须由 CLI 自己复述 —— 措辞与前台的 StartPortForwardsForeground 完全一致,同一件事不再有
// 两种说法。前台路径自己已告警,不在调用点重复。
// 只对 registered 里确实在听的地址告警:被 holder 拒绝(端口冲突、绑定失败)的规格从未监听,
// 报「listening on ...」是假告警,而紧随其后的拒绝告警已经说明了原因(address 生成方式与
// holder 的 opRegister 相同,故可直接比对)。
func warnNonLoopbackForwards(specs []*PortForwardSpec, registered []string) {
	listening := make(map[string]struct{}, len(registered))
	for _, a := range registered {
		listening[a] = struct{}{}
	}
	for _, spec := range specs {
		if isLoopbackAddr(spec.BindHost) {
			continue
		}
		addr := fmt.Sprintf("%s:%d", spec.BindHost, spec.LocalPort)
		if _, ok := listening[addr]; !ok {
			continue
		}
		fmt.Fprintf(os.Stderr, "WARNING: listening on %q (non-loopback). This port has no auth.\n", addr)
	}
}

// runTunnelLifecycle 对解析出的隧道执行 start/restart(动作在 CLI 侧没有 create 语义,名字必须
// 已由 resolveLifecycle 解析出来,R1)。start 可以把 node 拉起来(与 `relay run -t tunnel` 一致);
// restart 在 node 未运行时直接报错 —— 没有可重开的东西,不隐式拉起(R4 隧道侧)。
func runTunnelLifecycle(verb lifecycleVerb, cfg *config.Config, res lifecycleTarget) error {
	t := res.State
	if verb == verbStart {
		return startTunnel(cfg, t.Egress, t.Listen, t.Restart, os.Stderr)
	}
	if !res.NodeRunning {
		return fmt.Errorf("local node is not running; nothing to restart for %s (start it with `relay node start`, then `relay start %s`)", t.Egress, t.Egress)
	}
	resp, err := sendTunnelControl(tunnelSockPath(), tunControlMsg{Op: tunOpRestart, Egress: t.Egress})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("restart tunnel %s: %s", t.Egress, resp.Error)
	}
	// 重开的是意图文件里记下的监听面:非 loopback 时与创建路径(warnw)一样由 CLI 复述告警,
	// 否则 `relay restart <egress>` 会静默地把一个无认证入口重新挂上网。
	if !isLoopbackAddr(t.Listen) {
		fmt.Fprint(os.Stderr, tunnelNonLoopbackWarning(t.Listen))
	}
	if resp.Warning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", resp.Warning)
	}
	fmt.Printf("tunnel %s restarted\n", t.Egress)
	return nil
}

// tunnelStatesForRead 返回本机 node 侧可读的隧道现状(KTD8 的隧道两级来源):node 在运行时问
// holder(内存态是权威,含已停止条目);node 未运行时降级读意图文件并把 state 一律标成 stopped
// (R19:没有任何进程持有监听,文件里残留的 running 已不再成立)。
func tunnelStatesForRead(statePath, sockPath string, nodeRunning bool) ([]tunnelState, error) {
	if !nodeRunning {
		states, err := loadTunnels(statePath)
		if err != nil {
			return nil, err
		}
		for i := range states {
			states[i].State = "stopped"
		}
		return states, nil
	}
	resp, err := sendTunnelControl(sockPath, tunControlMsg{Op: tunOpList})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return resp.Tunnels, nil
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
