package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/db"
	"github.com/kirikira/gsm2sip-server/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := db.OpenAndMigrate(ctx, databaseURL)
	if err != nil {
		logger.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	service := httpapi.New(database, logger)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	logger.Info("worker started", "tasks", "expire unclaimed SMS commands")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := service.ExpireQueued(ctx)
			if err != nil {
				logger.Error("command expiry pass failed", "error", err)
				continue
			}
			if count > 0 {
				logger.Info("expired unclaimed SMS commands", "count", count)
			}
		}
	}
}
