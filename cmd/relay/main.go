package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/config"
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
	pullCmd = kingpin.Command("pull", "Download single file from remote watch directory")
	pullWatch = pullCmd.Flag("watch", "Target watch ID").Short('w').Required().String()
	pullFile = pullCmd.Arg("filename", "Remote filename to download").Required().String()

	// List command - list remote directory
	listCmd = kingpin.Command("list", "List files in remote watch directory")
	listWatch = listCmd.Flag("watch", "Target watch ID").Short('w').Required().String()

	// Push command - upload files
	pushCmd = kingpin.Command("push", "Push file to remote watch directory")
	pushWatch = pushCmd.Flag("watch", "Target watch ID").Short('w').Required().String()
	pushSrc = pushCmd.Arg("source", "Source file to push").Required().String()

	// Exec command - command forwarding (requires watch running)
	execCmd = kingpin.Command("exec", "Forward command to remote backend")
	execWatch = execCmd.Flag("watch", "Target watch ID").Short('w').String()
	execCmdStr = execCmd.Arg("command", "Command to execute").Required().String()

	// Cleanup command - remove stale command files
	cleanupCmd = kingpin.Command("cleanup", "Remove stale command and result files from remote")
	cleanupWatch = cleanupCmd.Flag("watch", "Target watch ID").Short('w').Required().String()
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
	default:
		app.Usage(os.Args)
	}
}

func runWatch() {
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

	w, err := watcher.New(cfg)
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

	watchCfg, err := cfg.GetWatchByID(*pullWatch)
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
}

func runList() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchCfg, err := cfg.GetWatchByID(*listWatch)
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

func runPush() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	watchCfg, err := cfg.GetWatchByID(*pushWatch)
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

	var watchDir string
	if *execWatch != "" {
		watchCfg, err := cfg.GetWatchByID(*execWatch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
		watchDir = watchCfg.WatchDir
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
	if *execWatch != "" {
		watchCfg, err := cfg.GetWatchByID(*execWatch)
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

	result, err := b.Exec(ctx, *execCmdStr, watchDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result)
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