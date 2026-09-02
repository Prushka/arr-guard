package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (runErr error) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Println("usage: MODE=serve|unmatched|subtitles arr-guard")
		return nil
	}
	if len(args) > 0 {
		return errors.New("command-line modes are removed; set MODE=serve, MODE=unmatched, or MODE=subtitles")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("configuration loaded", "config", cfg.logValue())
	service, err := NewService(cfg, log)
	if err != nil {
		return err
	}
	// A media-file deletion is performed before the failure/blocklist and
	// replacement-search API calls. Keep a short, independent shutdown window
	// to finish those calls if the main context is canceled or a run returns
	// after an API error.
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if cleanupErr := service.CleanupPending(cleanupCtx); cleanupErr != nil {
			log.Error("pending remediation cleanup failed", "error", cleanupErr)
			if runErr == nil {
				runErr = cleanupErr
			} else {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
	}()
	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, client := range service.arr {
		if err := client.Test(testCtx); err != nil {
			return fmt.Errorf("connect to %s: %w", client.Kind(), err)
		}
	}
	if cfg.Mode == "subtitles" {
		return service.Audit(ctx)
	}
	if cfg.Mode == "unmatched" {
		return service.ScanUnmatched(ctx)
	}
	service.StartWorkers(ctx)
	defer service.StopWorkers()
	return service.Serve(ctx)
}
