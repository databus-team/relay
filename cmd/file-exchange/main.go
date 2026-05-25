package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/file-exchange/internal/backend"
	"github.com/user/file-exchange/internal/config"
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
	pushCmd = kingpin.Command("push", "Push file to remote directory")
	pushDest = pushCmd.Flag("dest", "Destination directory on remote").Short('d').Required().String()
	pushSrc = pushCmd.Arg("source", "Source file to push").Required().String()

	// Exec command
	execCmd = kingpin.Command("exec", "Execute command locally")
	execCmdStr = execCmd.Arg("command", "Command to execute").Required().String()
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

	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create backend: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	src := *pushSrc
	dest := *pushDest + "/" + filepath.Base(src)

	info, err := os.Stat(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stat source: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		pushDir(ctx, b, src, *pushDest)
	} else {
		pushFile(ctx, b, src, dest)
	}

	fmt.Println("Push completed successfully")
}

func runExec() {
	// Execute command locally in current working directory
	cmd := exec.Command("sh", "-c", *execCmdStr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "Exec failed: %v\n", err)
		os.Exit(1)
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