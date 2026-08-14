package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
	"github.com/user/relay/internal/exchange"
	"github.com/user/relay/internal/jobrunner"
	_ "github.com/user/relay/internal/relay/backend"
	_ "github.com/user/relay/internal/relay/server"
	"github.com/user/relay/internal/watcher"
)

var (
	version = "1.0.0"

	app = kingpin.New("relay", "Generic File Exchange Command Execution System")

	_ = kingpin.CommandLine

	configPath = kingpin.Flag("config", "Path to config file").Short('c').Default("~/.relay/config.yaml").String()
	debugFlag  = kingpin.Flag("debug", "Enable debug mode").Bool()

	// Watch command - continuous monitoring
	watchCmd = kingpin.Command("watch", "Watch remote directory and execute actions continuously")

	// Pull command - download single file (requires filename)
	pullCmd    = kingpin.Command("pull", "Download single file from remote watch directory")
	pullWatch  = pullCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	pullFile   = pullCmd.Arg("filename", "Remote filename to download").Required().String()
	pullDelete = pullCmd.Flag("delete", "Delete the remote file after a successful pull").Short('d').Bool()

	// List command - list remote directory
	listCmd   = kingpin.Command("list", "List files in remote watch directory")
	listWatch = listCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()

	// Push command - upload files
	pushCmd   = kingpin.Command("push", "Push file to remote watch directory")
	pushWatch = pushCmd.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	pushSrc   = pushCmd.Arg("source", "Source file to push").Required().String()

	// Job command - run config-defined jobs locally
	jobCmd      = kingpin.Command("job", "Run config-defined jobs on the local machine")
	jobRun      = jobCmd.Command("run", "Run a config job locally")
	jobRunWatch = jobRun.Flag("watch", "Target watch ID (defaults to current directory name)").Short('w').String()
	jobRunID    = jobRun.Arg("jobid", "Job ID to run").Required().String()
	jobRunFile  = jobRun.Arg("file", "Local file to bind to {file_path} and friends").String()

	// Exec command - command forwarding (requires watch running)
	execCmd    = kingpin.Command("exec", "Forward command to remote backend")
	execWatch  = execCmd.Flag("watch", "Target watch ID").Short('w').String()
	execCmdStr = execCmd.Arg("command", "Command to execute").Required().String()

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
)

func main() {
	kingpin.CommandLine.HelpFlag.Short('h')

	if *debugFlag {
		backend.SetDebug(true)
	}

	switch kingpin.Parse() {
	case watchCmd.FullCommand():
		runWatch()
	case pullCmd.FullCommand():
		runPull()
	case pushCmd.FullCommand():
		runPush()
	case execCmd.FullCommand():
		runExec()
	case listCmd.FullCommand():
		runList()
	case cleanupCmd.FullCommand():
		runCleanup()
	case "help":
		app.Usage(os.Args)
	case syncCmd.FullCommand():
		runSync()

	case serverCmd.FullCommand():
		runServer()
	case wsCmd.FullCommand():
		runWorkspaces()
	case jobRun.FullCommand():
		runJobRun()
	default:
		app.Usage(os.Args)
	}
}

func runWatch() {
	ctx := context.Background()
	cfg, err := config.Load(*configPath)
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

	w, err := watcher.New(cfg, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create watcher: %v\n", err)
		os.Exit(1)
	}

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

	watchID, err := resolveWorkspaceID(cfg, *pullWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}
	watchCfg, err := cfg.GetWatchByID(watchID)
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

	watchID, err := resolveWorkspaceID(cfg, *listWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}
	watchCfg, err := cfg.GetWatchByID(watchID)
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

	available := make([]string, 0, len(cfg.Watch))
	for _, w := range cfg.Watch {
		available = append(available, w.ID+" ("+w.LocalDir+")")
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no workspace matches current directory %q; specify -w. Available: %s", base, strings.Join(available, ", "))
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("current directory %q matches multiple workspaces (%s); specify -w. Available: %s", base, strings.Join(matches, ", "), strings.Join(available, ", "))
	}
	return matches[0], nil
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
			Path:     j.Path,
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

	watchID, err := resolveWorkspaceID(cfg, *pushWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}
	watchCfg, err := cfg.GetWatchByID(watchID)
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
	dest := watchDir + "/" + filename

	info, err := os.Stat(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stat source: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		pushDir(ctx, b, src, watchDir)
	} else {
		pushFile(ctx, b, src, dest)
	}

	fmt.Println("Push completed successfully")
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

	// Health check: verify remote watcher is running
	if w != "" {
		watchCfg, err := cfg.GetWatchByID(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}

		fmt.Print("Checking remote watcher... ")
		if err := b.Ping(ctx, commandDir, watchCfg.ID); err != nil {
			fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("OK")
	} else {
		fmt.Println("Note: Specify -w to check remote watcher before exec")
	}

	result, err := b.Exec(ctx, *execCmdStr, execCwd, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result)
}

func runJobRun() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchID, err := resolveWorkspaceID(cfg, *jobRunWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}
	watchCfg, err := cfg.GetWatchByID(watchID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		os.Exit(1)
	}

	res, err := jobrunner.Run(context.Background(), watchCfg, *jobRunID, *jobRunFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Job error: %v\n", err)
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
		os.Exit(1)
	}

	if res.Stdout != "" {
		fmt.Print(res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
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
	commandDir := "/commands"
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
