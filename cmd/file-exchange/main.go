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
	"github.com/user/file-exchange/internal/watcher"
)

var (
	version = "1.0.0"

	app = kingpin.New("file-exchange", "Generic File Exchange Command Execution System")

	_ = kingpin.CommandLine

	configPath = kingpin.Flag("config", "Path to config file").Short('c').Default("~/.file-exchange/config.yaml").String()

	watchCmd = kingpin.Command("watch", "Watch remote directory and execute actions")

	watchOnce = watchCmd.Flag("once", "Run watch loop only once instead of daemon mode").Bool()

	pushCmd = kingpin.Command("push", "Push files to remote server")

	pushSrc = pushCmd.Arg("source", "Source file or directory to push").Required().String()
	pushDest = pushCmd.Arg("dest", "Destination path on remote server").Required().String()
)

func main() {
	kingpin.CommandLine.HelpFlag.Short('h')

	switch kingpin.Parse() {
	case watchCmd.FullCommand():
		runWatch()
	case pushCmd.FullCommand():
		runPush()
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

	// Validate backend and job compatibility
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Config validation failed: %v\n", err)
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

func validateConfig(cfg *config.Config) error {
	b, err := backend.NewBackend(cfg.Backend.Type, cfg.Backend.Config)
	if err != nil {
		return fmt.Errorf("failed to create backend: %w", err)
	}

	supportsExec := b.SupportsExec()

	for _, wc := range cfg.Watchers {
		for _, job := range wc.Jobs {
			if job.Type == "exec" && !supportsExec {
				return fmt.Errorf("backend type %q does not support exec action; use fs-mcp or local backend", cfg.Backend.Type)
			}
		}
	}

	return nil
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
	dest := *pushDest

	// Check if source is a directory
	info, err := os.Stat(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stat source: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		pushDir(ctx, b, src, dest)
	} else {
		pushFile(ctx, b, src, dest)
	}

	fmt.Println("Push completed successfully")
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

	fmt.Printf("Pushed file: %s -> %s\n", src, dest)
}

func pushDir(ctx context.Context, b backend.FileTransferBackend, src, dest string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read source directory: %v\n", err)
		os.Exit(1)
	}

	// Ensure dest ends with /
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