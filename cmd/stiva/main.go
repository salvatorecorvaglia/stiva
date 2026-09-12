// Stiva — Self-hosted S3-compatible Object Storage
//
// Stiva is a high-performance, S3-compatible object storage server
// with a built-in web console for bucket and object management.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/config"
	"github.com/salvatorecorvaglia/stiva/internal/server"
)

var (
	version = "2.0.0"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// run returns an error rather than calling os.Exit directly so that deferred
	// cleanup (notably the shutdown context cancel) always runs.
	if err := run(); err != nil {
		slog.Error("Stiva exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse version flags
	for _, arg := range os.Args[1:] {
		if arg == "-v" || arg == "--version" || arg == "-version" || arg == "version" {
			fmt.Printf("Stiva version %s (commit %s, built at %s)\n", version, commit, date)
			return nil
		}
	}

	cfg := config.Load()

	// Set up structured logging
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(cfg.LogFormat) == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))

	// Fail fast on configuration that would leave the server broken or
	// insecure, rather than surfacing it as confusing runtime behaviour.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	if cfg.AccessKey == "stiva" && cfg.SecretKey == "stiva123" {
		slog.Warn("WARNING: Running Stiva with default credentials! Please set STIVA_ACCESS_KEY and STIVA_SECRET_KEY in production.")
	}

	printBanner(cfg)

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to start Stiva: %w", err)
	}

	if err := srv.Start(); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	slog.Info("Shutdown signal received")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}

	slog.Info("[Server] Stiva stopped")
	return nil
}

func printBanner(cfg *config.Config) {
	banner := fmt.Sprintf(`
  ____  _   _             
 / ___|| |_(_)_   ____ _  
 \___ \| __| \ \ / / _`+"`"+`| 
  ___) | |_| |\ V / (_| | 
 |____/ \__|_| \_/ \__,_| 

  S3-Compatible Object Storage Server (%s)
`, version)
	fmt.Print(banner)
	scheme := "http"
	if cfg.TLSEnabled {
		scheme = "https"
	}
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  S3 API:     %s://0.0.0.0:%d\n", scheme, cfg.S3Port)
	fmt.Printf("  Console:    %s://0.0.0.0:%d\n", scheme, cfg.ConsolePort)
	fmt.Printf("  Data Dir:   %s\n", cfg.DataDir)
	fmt.Printf("  Access Key: %s\n", cfg.AccessKey)
	fmt.Printf("  Region:     %s\n", cfg.Region)
	fmt.Println("─────────────────────────────────────────")
	fmt.Println()
}
