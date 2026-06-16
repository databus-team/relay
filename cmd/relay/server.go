package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/relay/server"
)

var (
	serverCmd = kingpin.Command("server", "Run relay server")
	
	serverAddr = serverCmd.Flag("addr", "Server listen address").Default(":8443").String()
	serverWatchDirs = serverCmd.Flag("watch", "Watch directory (format: id:path)").Strings()
	serverToken = serverCmd.Flag("token", "Authentication token").String()
	serverTLSCert = serverCmd.Flag("tls-cert", "TLS certificate file").String()
	serverTLSKey = serverCmd.Flag("tls-key", "TLS key file").String()
)

func runServer() error {
	watchDirs := make([]server.WatchDirConfig, 0)
	for _, wd := range *serverWatchDirs {
		var id, path string
		fmt.Sscanf(wd, "%[^:]:%s", &id, &path)
		if id == "" || path == "" {
			fmt.Fprintf(os.Stderr, "Invalid watch format: %s (expected id:path)\n", wd)
			continue
		}
		watchDirs = append(watchDirs, server.WatchDirConfig{ID: id, Dir: path})
	}
	
	cfg := server.Config{
		Addr:      *serverAddr,
		WatchDirs: watchDirs,
		Auth:      server.AuthConfig{Type: "token", Tokens: []string{*serverToken}},
	}
	
	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()
	
	fmt.Printf("Starting relay server on %s\n", *serverAddr)
	for _, wd := range watchDirs {
		fmt.Printf("  Watching: %s -> %s\n", wd.ID, wd.Dir)
	}
	
	return srv.Serve(ctx)
}
