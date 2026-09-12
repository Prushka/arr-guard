package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Prushka/arr-guard/internal/config"
)

func Run(ctx context.Context, args []string) (runErr error) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Println("usage: MODE=serve|unmatched|subtitles arr-guard")
		return nil
	}
	if len(args) > 0 {
		return errors.New("command-line modes are removed; set MODE=serve, MODE=unmatched, or MODE=subtitles")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("configuration loaded", "config", cfg.LogValue())
	service, err := NewService(cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = service.state.Close() }()
	// Never replay a possibly committed mutation during shutdown.
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
	return service.Serve(ctx)
}
