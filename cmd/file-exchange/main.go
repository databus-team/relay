package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
)

func main() {
	kingpin.CommandLine.HelpFlag.Short('h')

	switch kingpin.Parse() {
	case watchCmd.FullCommand():
		runWatch()
	case pushCmd.FullCommand():
		fmt.Println("push command not yet implemented")
		os.Exit(1)
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