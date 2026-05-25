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
	"github.com/user/file-exchange/internal/backend"
	"github.com/user/file-exchange/internal/config"
	"github.com/user/file-exchange/internal/exchange"
	"github.com/user/file-exchange/internal/watcher"
)

var (
	version = "1.0.0"

	app = kingpin.New("file-exchange", "Generic File Exchange Command Execution System")

	_ = kingpin.CommandLine

	configPath = kingpin.Flag("config", "Path to config file").Short('c').Default("~/.file-exchange/config.yaml").String()

	// Watch command
	watchCmd = kingpin.Command("watch", "Watch remote directory and execute actions")
	watchOnce = watchCmd.Flag("once", "Run watch loop only once instead of daemon mode").Bool()

	// Push command
	pushCmd = kingpin.Command("push", "Push file to remote device")
	pushDevice = pushCmd.Flag("device", "Target device ID").Short('d').Required().String()
	pushSrc = pushCmd.Arg("source", "Source file to push").Required().String()

	// Exec command
	execCmd = kingpin.Command("exec", "Execute command on remote device")
	execDevice = execCmd.Flag("device", "Target device ID").Short('d').Required().String()
	execCmdStr = execCmd.Arg("command", "Command to execute").Required().String()
	execCwd = execCmd.Flag("cwd", "Working directory").String()
)

func main() {
	kingpin.CommandLine.HelpFlag.Short('h')

	switch kingpin.Parse() {
	case watchCmd.FullCommand():
		runWatch()
	case pushCmd.FullCommand():
		runPush()
	case execCmd.FullCommand():
		runExec()
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

	if *watchOnce {
		if err := w.RunOnce(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := w.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
			os.Exit(1)
		}
	}
}

func runPush() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	device, err := cfg.GetDevice(*pushDevice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Device error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(device.Backend.Type, device.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	src := *pushSrc
	dest := device.Paths.WatchDir + "/" + filepath.Base(src)

	info, err := os.Stat(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stat source: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		pushDirToDevice(ctx, b, src, device.Paths.WatchDir)
	} else {
		pushFileToDevice(ctx, b, src, dest)
	}

	fmt.Println("Push completed successfully")
}

func pushFileToDevice(ctx context.Context, b backend.FileTransferBackend, src, dest string) {
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

func pushDirToDevice(ctx context.Context, b backend.FileTransferBackend, src, dest string) {
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
			pushDirToDevice(ctx, b, srcPath, destPath)
		} else {
			pushFileToDevice(ctx, b, srcPath, destPath)
		}
	}
}

func runExec() {
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	device, err := cfg.GetDevice(*execDevice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Device error: %v\n", err)
		os.Exit(1)
	}

	b, err := backend.NewBackend(device.Backend.Type, device.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	if !b.SupportsExec() {
		fmt.Fprintf(os.Stderr, "Device %q does not support exec action\n", *execDevice)
		os.Exit(1)
	}

	ctx := context.Background()
	cmd := *execCmdStr
	cwd := *execCwd
	if cwd == "" {
		cwd = "/"
	}

	ex := exchange.NewFileExchange(b, b.GetCommandDir())
	result, err := ex.ExecuteCommand(ctx, cmd, cwd, 300)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec failed: %v\n", err)
		os.Exit(1)
	}

	if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "Command exited with code %d:\n%s\n", result.ExitCode, result.Stderr)
		os.Exit(1)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprintf(os.Stderr, "\nStderr:\n%s\n", result.Stderr)
	}
}