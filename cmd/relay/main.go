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
	"github.com/user/relay/internal/version"
	"github.com/user/relay/internal/watcher"
)

var (
	app = kingpin.New("relay", "Generic File Exchange Command Execution System")

	_ = kingpin.CommandLine

	configPath = kingpin.Flag("config", "Path to config file").Short('c').Default("~/.relay/config.yaml").String()
	debugFlag  = kingpin.Flag("debug", "Enable debug mode").Bool()

	// Watch command - continuous monitoring
	watchCmd = kingpin.Command("watch", "Watch remote directory and execute actions continuously")

	// 位置参数 action:默认前台;run=前台;start/stop/status/restart/upgrade 为 daemon 控制。
	watchAction = watchCmd.Arg("action", "run|start|stop|status|restart|upgrade (default: run)").HintOptions("run", "start", "stop", "status", "restart", "upgrade").String()
	// upgrade 用的新二进制路径(仅 action=upgrade 时使用)。
	watchUpgradePath = watchCmd.Arg("upgrade-path", "Path to new relay binary (with action=upgrade)").String()

	// Pull command - download single file (requires filename)
	pullCmd    = kingpin.Command("pull", "Download single file from remote watch directory")
	pullWatch  = pullCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	pullFile   = pullCmd.Arg("filename", "Remote filename to download").Required().String()
	pullDelete = pullCmd.Flag("delete", "Delete the remote file after a successful pull").Short('d').Bool()

	// List command - list remote directory
	listCmd   = kingpin.Command("list", "List files in remote watch directory")
	listWatch = listCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()

	// Push command - upload files, optionally without running workspace jobs
	pushCmd    = kingpin.Command("push", "Push file to remote (optionally run/wait workspace jobs)")
	pushWatch  = pushCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	pushSrc    = pushCmd.Arg("source", "Source file to push").Required().String()
	pushDest   = pushCmd.Flag("dest", "Destination absolute path on the executor (defaults to <watch_dir>/<filename>; requires --no-jobs)").String()
	pushNoJobs = pushCmd.Flag("no-jobs", "Transfer only; do not run workspace jobs on the remote").Bool()

	// Job command - run config-defined jobs locally
	jobCmd      = kingpin.Command("job", "Run config-defined jobs on the local machine")
	jobRun      = jobCmd.Command("run", "Run a config job locally")
	jobRunWatch = jobRun.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	jobRunID    = jobRun.Arg("jobid", "Job ID to run").Required().String()
	jobRunFile  = jobRun.Arg("file", "Local file to bind to {file_path} and friends").String()

	// Exec command - command forwarding (requires watch running)
	execCmd   = kingpin.Command("exec", "Forward command to remote backend")
	execWatch = execCmd.Flag("watch", "Target watch ID").Short('w').String()
	// 支持多词命令:relay exec git status 会收成 ["git","status"] 再空格 join。
	// 含以 - 开头参数的命令(如 git log --oneline)仍须整体加引号,避免被当 flag。
	execCmdStr = execCmd.Arg("command", "Command to execute (multiple words are joined)").Required().Strings()

	// Status command - 一站式连通性体检:本地→中转→执行方 三段时延 + 各端点版本台账。
	// 继承原 `ping` 的探活职责并把 `version -r` 的远端台账职责一并归一到此命令。
	statusCmd     = kingpin.Command("status", "一站式连通性体检:本地→中转→执行方 三段时延与各端点版本")
	statusWatch   = statusCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	statusJSONOut = statusCmd.Flag("json", "Output as JSON").Bool()

	// Cleanup command - remove stale command files
	cleanupCmd   = kingpin.Command("cleanup", "Remove stale command and result files from remote")
	cleanupWatch = cleanupCmd.Flag("watch", "Target watch ID").Short('w').Required().String()

	// Sync command - push config to remote watcher for hot reload
	syncCmd = kingpin.Command("sync", "Push config to remote watcher for hot reload")

	// Workspaces command - list configured workspaces from config
	// (alias: `workspaces`; kingpin v2 doesn't render aliases in --help)
	wsCmd     = kingpin.Command("ws", "List configured workspaces from config (alias: workspaces)").Alias("workspaces")
	wsName    = wsCmd.Flag("name", "Show details for a specific workspace ID").String()
	wsJSON    = wsCmd.Flag("json", "Output as JSON").Bool()
	wsVerbose = wsCmd.Flag("verbose", "Show detailed table output").Short('v').Bool()

	// Version command - 版本查看(纯本地构建信息)。远端台账职责已归一到 `status`。
	versionCmd     = kingpin.Command("version", "显示本机构建信息")
	versionJSONOut = versionCmd.Flag("json", "Output as JSON").Bool()

	// server-remote command - 一键部署中转(受控自升级):上传新二进制 → 中转自检 → 换装 → 核验。
	serverRemoteCmd = kingpin.Command("server-remote", "一键部署中转:上传新 relay 二进制并经中转受控自升级,断线重连后核验版本")
	serverRemoteBin = serverRemoteCmd.Flag("binary", "Path to the new relay binary to send (default: current executable)").String()
)

func main() {
	kingpin.CommandLine.HelpFlag.Short('h')

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
	case statusCmd.FullCommand():
		runStatus()
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
	case serverRemoteCmd.FullCommand():
		runServerRemote()
	case jobRun.FullCommand():
		runJobRun()
	default:
		app.Usage(os.Args)
	}
}

// normalizedAction 把 action 位置参数归一:run 或空视为前台(返回 ""),其余 daemon 动作原样返回。
func normalizedAction(a string) string {
	switch a {
	case "run", "":
		return ""
	case "start", "stop", "status", "restart", "upgrade":
		return a
	default:
		return ""
	}
}

// daemonStart 已运行则提示,否则 detached 拉起 `relay <name> run -c <config>` 并记录 pid。
func daemonStart(name string) error {
	pidFile, logFile := daemon.PidFile(name), daemon.LogFile(name)
	st := daemon.Get(pidFile)
	if st.Err != nil {
		return st.Err
	}
	if st.Running {
		fmt.Printf("%s already running (pid %d)\n", name, st.Pid)
		return nil
	}
	pid, err := daemon.Start([]string{name, "run", "-c", *configPath}, logFile, pidFile)
	if err != nil {
		return fmt.Errorf("daemon start %s: %w", name, err)
	}
	fmt.Printf("%s started (pid %d)\nlog: %s\n", name, pid, logFile)
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
	pidFile, logFile := daemon.PidFile(name), daemon.LogFile(name)
	st := daemon.Get(pidFile)
	if st.Err == nil && st.Running {
		_ = daemon.Stop(pidFile)
	}
	pid, err := daemon.Start([]string{name, "run", "-c", *configPath}, logFile, pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Printf("%s restarted (pid %d)\nlog: %s\n", name, pid, logFile)
}

// daemonUpgrade 按需升级:停 daemon → 原子替换自身二进制 → 用新二进制重启。
// 这是中转/执行方"self-update + reboot"的入口,仅在被调用时动作(非常驻)。
func daemonUpgrade(name, newBin string) {
	if newBin == "" {
		fmt.Fprintf(os.Stderr, "Error: action 'upgrade' requires a binary path argument\n")
		return
	}
	pidFile, logFile := daemon.PidFile(name), daemon.LogFile(name)
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
	pid, err := daemon.Start([]string{name, "run", "-c", *configPath}, logFile, pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: restart %s: %v\n", name, err)
		return
	}
	fmt.Printf("%s upgraded & restarted (pid %d, binary=%s)\nlog: %s\n", name, pid, newBin, logFile)
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
// returns the full list. Errors propagate from config.GetWatchByID (e.g. "watch
// not found: <id>") so callers can surface them with their own exit handling.
func resolveWatches(cfg *config.Config, name string) ([]config.WatchConfig, error) {
	if name == "" {
		return cfg.Watch, nil
	}
	w, err := cfg.GetWatchByID(name)
	if err != nil {
		return nil, err
	}
	return []config.WatchConfig{*w}, nil
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
	for _, w := range cfg.Watch {
		if w.ID == base {
			matches = append(matches, w.ID)
		}
	}

	if len(matches) > 1 {
		return "", fmt.Errorf("current directory %q matches multiple workspaces (%s); specify -w. Available: %s", base, strings.Join(matches, ", "), joinAvailable(cfg.Watch))
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no workspace matches current directory %q; specify -w. Available: %s", base, joinAvailable(cfg.Watch))
	}
	return matches[0], nil
}

// joinAvailable renders the configured workspaces as "id (local_dir)" for
// error messages. It is only invoked on the error paths.
func joinAvailable(watches []config.WatchConfig) string {
	parts := make([]string, 0, len(watches))
	for _, w := range watches {
		parts = append(parts, w.ID+" ("+w.LocalDir+")")
	}
	return strings.Join(parts, ", ")
}

// lookupWatch resolves an optional -w value (or the cwd-inferred workspace) to
// its WatchConfig. It separates workspace resolution from the commands that
// just need the config.
func lookupWatch(cfg *config.Config, provided string) (*config.WatchConfig, error) {
	watchID, err := resolveWorkspaceID(cfg, provided)
	if err != nil {
		return nil, err
	}
	return cfg.GetWatchByID(watchID)
}

// workspaceJSON is the on-the-wire shape for `relay ws --json`. It mirrors
// config.WatchConfig but uses lower-case JSON keys so consumers can pipe into
// jq / scripts without depending on Go's default field capitalization.
type workspaceJSON struct {
	ID       string          `json:"id"`
	WatchDir string          `json:"watch_dir"`
	LocalDir string          `json:"local_dir"`
	Paths    []string        `json:"paths"`
	Jobs     []jobConfigJSON `json:"jobs"`
}

type jobConfigJSON struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Cmd      string `json:"cmd,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Path     string `json:"path,omitempty"`
	If       string `json:"if,omitempty"`
	KeepFile bool   `json:"keep_file"`
	Timeout  int    `json:"timeout,omitempty"`
}

func toWorkspaceJSON(w config.WatchConfig) workspaceJSON {
	jobs := make([]jobConfigJSON, len(w.Jobs))
	for i, j := range w.Jobs {
		jobs[i] = jobConfigJSON{
			ID:       j.ID,
			Type:     j.Type,
			Cmd:      j.Cmd,
			Cwd:      j.Cwd,
			If:       j.If,
			KeepFile: j.KeepFile,
			Timeout:  j.Timeout,
		}
	}
	return workspaceJSON{
		ID:       w.ID,
		WatchDir: w.WatchDir,
		LocalDir: w.LocalDir,
		Paths:    w.Paths,
		Jobs:     jobs,
	}
}

// printWorkspacesTable renders a 4-column summary of configured workspaces
// with column widths sized to the longest cell in each column (subject to a
// min width equal to the column header). A row that exceeds its column width
// is truncated by fmt's %-*s.
func printWorkspacesTable(watches []config.WatchConfig) {
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

	watchCfg, err := lookupWatch(cfg, *pushWatch)
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
	src := *pushSrc

	watchDir := watchCfg.WatchDir
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

	dest := watchDir + "/" + filename
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

	// 纯下发(不跑 workspace job): 走 Jobs=false 通道,落到 dest(绝对)或 watch_dir 目录。
	if *pushNoJobs {
		if tn, ok := b.(backend.PushNoJobsSender); ok {
			exit, perr := tn.PushNoJobs(ctx, dest, readContent())
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

	// 直达后端(relay):把文件直达远端执行方并触发其本地 jobs;输出流式显示。
	if pj, ok := b.(backend.PushJobSender); ok {
		exit, perr := pj.PushJob(ctx, dest, readContent(), func(c backend.ExecChunk) {
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
		out(backend.ExecChunk{Stdout: false, Data: fmt.Sprintf("push-job: unknown watch %q\n", watchID)})
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
// 单根模型下执行方的 sess.WatchID 是根 watch_id(如 "storage"),并非 workspace id;
// 故先按 watchID 精确匹配,失败则从落盘路径 absPath 推导:取路径中与某 workspace
// 的 watch_dir 或 id 相等的段(路径形如 <executor_root>/<workspace>/<file>)。
func resolvePushWorkspace(cfg *config.Config, watchID, absPath string) *config.WatchConfig {
	if wc, err := cfg.GetWatchByID(watchID); err == nil {
		return wc
	}
	segs := strings.Split(strings.ReplaceAll(absPath, "\\", "/"), "/")
	for i := range cfg.Watch {
		wc := &cfg.Watch[i]
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

	// Fold workspace resolution: an explicit -w wins; otherwise try to infer it
	// from the cwd basename. Inferring is best-effort for exec — when it can't
	// resolve, exec keeps its existing no-workspace forwarding behavior.
	w := *execWatch
	if w == "" {
		if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
			w = inferred
		}
	}

	var execCwd string
	if w != "" {
		watchCfg, err := cfg.GetWatchByID(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		execCwd = watchCfg.LocalDir
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
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
		watchCfg, err := cfg.GetWatchByID(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		if err := b.Ping(ctx, commandDir, watchCfg.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Remote watcher check failed: %v\n", err)
			os.Exit(1)
		}
	}

	cmd := strings.Join(*execCmdStr, " ")

	// 支持流式的后端(relay)逐帧实时转发输出,并以 exit code 收尾
	if eb, ok := b.(backend.ExecStreamBackend); ok {
		exit, err := eb.ExecStream(ctx, cmd, execCwd, 0, func(chunk backend.ExecChunk) {
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

// runPing 探活远端 watcher。workspace 解析:显式 -w 优先,否则按 cwd 推断,
// 推断失败则报可用清单退出(与 exec 的折叠逻辑一致)。
// statusTimeout 是 `relay status` 单条命令的上限(含对中转 status 请求的等待)。
const statusTimeout = 15 * time.Second

// runStatus 一站式连通性体检:按 watch 输出 本地→中转 / 中转→执行方 / 本地累计 三段时延,
// 并附各端点版本台账。relay 后端给出完整三段;其余后端(local/fs-mcp/jumpserver)尽力而为,
// 只有单跳段可达,段2/累计标「不可用」,保持相同列结构与 --json 字段。
func runStatus() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	w := *statusWatch
	if w == "" {
		if inferred, err := resolveWorkspaceID(cfg, ""); err == nil {
			w = inferred
		}
	}
	if w == "" {
		fmt.Fprintf(os.Stderr, "Specify -w. Available: %s\n", joinAvailable(cfg.Watch))
		os.Exit(1)
	}
	watchCfg, err := cfg.GetWatchByID(w)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	// relay 后端专属:完整三段 + 版本台账。
	if rb, ok := b.(*relaybackend.RelayBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		st, err := rb.Status(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		printStatus(watchCfg.ID, cfg.Backend.Type, st, *statusJSONOut)
		return
	}

	// 非 relay 端点:单跳可达(沿用 runPing 的 b.Ping),段2/累计明确标不可用。
	commandDir := "/tmp/relay-commands"
	if dir, ok := cfg.Backend.Config["command_dir"].(string); ok && dir != "" {
		commandDir = dir
	}
	start := time.Now()
	if err := b.Ping(context.Background(), commandDir, watchCfg.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	ms := time.Since(start).Milliseconds()
	reason := "not supported on " + cfg.Backend.Type + " backend"
	st := protocol.StatusResponse{
		OK:    true,
		Seg1:  protocol.StatusSegment{LatencyMS: &ms},
		Seg2:  protocol.StatusSegment{Unavailable: reason},
		Total: protocol.StatusSegment{Unavailable: reason},
	}
	printStatus(watchCfg.ID, cfg.Backend.Type, st, *statusJSONOut)
}

// printStatus 渲染 `relay status` 结果:--json 输出与文本一致的相同字段,段不可用时 latency_ms
// 留空并给出 unavailable 原因。
func printStatus(watchID, backendName string, st protocol.StatusResponse, jsonOut bool) {
	if jsonOut {
		rep := map[string]interface{}{
			"watch":   watchID,
			"backend": backendName,
			"seg1":    st.Seg1,
			"seg2":    st.Seg2,
			"total":   st.Total,
			"nodes":   st.Nodes,
		}
		enc, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(enc))
		return
	}

	fmt.Printf("status %q (%s backend)\n", watchID, backendName)
	fmt.Printf("  local→transit     %s\n", segmentText(st.Seg1))
	fmt.Printf("  transit→executor  %s\n", segmentText(st.Seg2))
	fmt.Printf("  local total       %s\n", segmentText(st.Total))
	if len(st.Nodes) == 0 {
		fmt.Println("  endpoints: (none reported)")
		return
	}
	fmt.Println("  endpoints:")
	for _, n := range st.Nodes {
		if n.Role == "transit" {
			fmt.Printf("    transit   %s  (%s/%s)\n", ident(n), n.GOOS, n.GOARCH)
		} else {
			fmt.Printf("    executor  watch=%s  %s\n", n.WatchID, ident(n))
		}
	}
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

	fmt.Println("[server-remote] 中转已回 ACK(自检通过); 正在轮询账户版本核验 ...")
	if err := pollTransitVersion(rb, version.String()); err != nil {
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
func jobTypeOf(watchCfg *config.WatchConfig, jobID string) string {
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

	_, err = cfg.GetWatchByID(*cleanupWatch)
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
	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	// 通道遵循 backend:支持流式的后端(relay)经 WS 直达执行方落盘,不绕中转文件交换。
	if cs, ok := b.(backend.ConfigSyncCapable); ok {
		fmt.Printf("Syncing config via %s streaming channel...\n", cfg.Backend.Type)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		exit, err := cs.ConfigSync(ctx, configData)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Sync failed: %v\n", err)
			os.Exit(1)
		}
		if exit != 0 {
			fmt.Fprintf(os.Stderr, "Sync failed on executor (exit %d)\n", exit)
			os.Exit(1)
		}
		fmt.Println("Sync successful (config written on executor; restart relay watch to take effect)")
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
