package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/db"
	"github.com/kirikira/gsm2sip-server/internal/httpapi"
	"github.com/kirikira/gsm2sip-server/internal/push"
	"github.com/kirikira/gsm2sip-server/internal/securestore"
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
	options := httpapi.Options{}
	for _, name := range []string{"ARI_URL", "SIP_SERVER_NAME"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			logger.Warn("SIP calling was removed in 0.4; ignoring the setting", "setting", name)
		}
	}
	// SIP_COMPATIBILITY_ENABLED=false is kept in deployments so a rollback
	// to an older release stays on RTC; only asking for SIP is worth a warning.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SIP_COMPATIBILITY_ENABLED")), "true") {
		logger.Warn("SIP calling was removed in 0.4; ignoring the setting", "setting", "SIP_COMPATIBILITY_ENABLED")
	}
	options.RTC.STUNURLs = strings.FieldsFunc(os.Getenv("RTC_STUN_URLS"), func(c rune) bool { return c == ',' || c == ' ' })
	options.RTC.TURNURLs = strings.FieldsFunc(os.Getenv("RTC_TURN_URLS"), func(c rune) bool { return c == ',' || c == ' ' })
	options.RTC.TURNSecret = os.Getenv("RTC_TURN_SHARED_SECRET")
	for _, url := range options.RTC.STUNURLs {
		if !strings.HasPrefix(url, "stun:") && !strings.HasPrefix(url, "stuns:") {
			logger.Error("Invalid RTC_STUN_URLS scheme")
			os.Exit(2)
		}
	}
	for _, url := range options.RTC.TURNURLs {
		if !strings.HasPrefix(url, "turn:") && !strings.HasPrefix(url, "turns:") {
			logger.Error("Invalid RTC_TURN_URLS scheme")
			os.Exit(2)
		}
	}
	if len(options.RTC.TURNURLs) > 0 && len(options.RTC.TURNSecret) < 32 {
		logger.Error("RTC TURN requires a shared secret of at least 32 bytes")
		os.Exit(2)
	}
	if encoded := strings.TrimSpace(os.Getenv("SECRETS_ENCRYPTION_KEY")); encoded != "" {
		key, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil || len(key) != 32 {
			logger.Error("SECRETS_ENCRYPTION_KEY must be base64 of 32 bytes")
			os.Exit(2)
		}
		options.SecretCipher, err = securestore.New(key)
		if err != nil {
			logger.Error("secret cipher configuration invalid")
			os.Exit(2)
		}
	}
	// Phones register their own push endpoints; the node needs no push
	// credentials. Private endpoints (a LAN UnifiedPush server) are opt-in
	// because the node would otherwise post to any address a phone names.
	options.Push = push.NewHTTP(strings.EqualFold(strings.TrimSpace(os.Getenv("PUSH_ALLOW_PRIVATE_ENDPOINTS")), "true"))
	api := httpapi.NewWithOptions(database, logger, options)
	go api.RunBackground(ctx)
	address := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if address == "" {
		address = ":8080"
	}
	server := &http.Server{
		Addr:              address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	go func() {
		logger.Info("API listening", "addr", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("API server failed", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	api.ShutdownWebSockets()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("API shutdown failed", "error", err)
	}
}
