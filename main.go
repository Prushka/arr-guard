package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, []string{mode}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	mode := "serve"
	if len(args) > 0 && args[0] != "" {
		mode = args[0]
	}
	if mode == "-h" || mode == "--help" || mode == "help" {
		fmt.Println("usage: arr-subtitle-guard [serve|audit]")
		return nil
	}
	if mode != "serve" && mode != "audit" {
		return fmt.Errorf("unknown mode %q (want serve or audit)", mode)
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
	if mode == "audit" {
		return service.Audit(ctx)
	}
	service.StartWorkers(ctx)
	defer service.StopWorkers()
	return service.Serve(ctx)
}
