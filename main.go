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

func run(ctx context.Context, args []string) error {
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
