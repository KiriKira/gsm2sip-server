package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/calls"
	"github.com/kirikira/gsm2sip-server/internal/db"
	"github.com/kirikira/gsm2sip-server/internal/httpapi"
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
	sipCompatibility := true
	if value := strings.TrimSpace(os.Getenv("SIP_COMPATIBILITY_ENABLED")); value != "" {
		sipCompatibility, err = strconv.ParseBool(value)
		if err != nil {
			logger.Error("SIP_COMPATIBILITY_ENABLED must be a boolean")
			os.Exit(2)
		}
	}
	if value := strings.TrimSpace(os.Getenv("RTC_MEDIA_ENABLED")); value != "" {
		options.RTC.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			logger.Error("RTC_MEDIA_ENABLED must be a boolean")
			os.Exit(2)
		}
	}
	if value := strings.TrimSpace(os.Getenv("RTC_CONTROL_ENABLED")); value != "" {
		options.RTC.ControlEnabled, err = strconv.ParseBool(value)
		if err != nil {
			logger.Error("RTC_CONTROL_ENABLED must be a boolean")
			os.Exit(2)
		}
	}
	if options.RTC.ControlEnabled && !options.RTC.Enabled {
		logger.Error("RTC_CONTROL_ENABLED requires RTC_MEDIA_ENABLED")
		os.Exit(2)
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
		if sipCompatibility {
			transport := strings.ToLower(strings.TrimSpace(os.Getenv("SIP_TRANSPORT")))
			if transport == "" {
				transport = httpapi.SIPTransportTLS
			}
			if transport != httpapi.SIPTransportTLS && transport != httpapi.SIPTransportUDP {
				logger.Error("SIP_TRANSPORT must be tls or udp")
				os.Exit(2)
			}
			port := 5061
			if transport == httpapi.SIPTransportUDP {
				port = 5060
			}
			if value := strings.TrimSpace(os.Getenv("SIP_PORT")); value != "" {
				port, err = strconv.Atoi(value)
				if err != nil {
					logger.Error("SIP_PORT is invalid")
					os.Exit(2)
				}
			}
			options.SIP = httpapi.SIPSettings{ServerName: strings.TrimSpace(os.Getenv("SIP_SERVER_NAME")), Port: port, Transport: transport}
			if value := strings.TrimSpace(os.Getenv("SIP_ENABLE_OPUS")); value != "" {
				var parseErr error
				options.SIP.EnableOpus, parseErr = strconv.ParseBool(value)
				if parseErr != nil {
					logger.Error("SIP_ENABLE_OPUS must be a boolean")
					os.Exit(2)
				}
			}
			if filename := strings.TrimSpace(os.Getenv("SIP_CA_PEM_FILE")); filename != "" {
				pem, readErr := os.ReadFile(filename)
				if readErr != nil || len(pem) > 256*1024 {
					logger.Error("SIP CA file cannot be read or is too large")
					os.Exit(2)
				}
				if !x509.NewCertPool().AppendCertsFromPEM(pem) {
					logger.Error("SIP CA file contains no valid certificates")
					os.Exit(2)
				}
				options.SIP.CAPEM = string(pem)
			}
			if options.SIP.ServerName != "" && !options.SIP.Valid() {
				logger.Error("SIP public address is invalid")
				os.Exit(2)
			}
			if ariURL := strings.TrimSpace(os.Getenv("ARI_URL")); ariURL != "" {
				if !options.SIP.Valid() {
					logger.Error("SIP settings are invalid")
					os.Exit(2)
				}
				// Domain separation avoids reusing the AES encryption key as a MAC key.
				tokenKey := sha256.Sum256(append([]byte("gsm2sip.call-intent-mac.v1\x00"), key...))
				application := strings.TrimSpace(os.Getenv("ARI_APPLICATION"))
				if application == "" {
					application = "gsm2sip"
				}
				manager, err := calls.NewManager(database, calls.Config{ARIURL: ariURL, ARIUsername: os.Getenv("ARI_USERNAME"), ARIPassword: os.Getenv("ARI_PASSWORD"), ARIApplication: application, SIPRealm: "gsm2sip", TokenKey: tokenKey[:], SIPInsecure: !options.SIP.Secure()})
				if err != nil {
					logger.Error("ARI coordinator configuration is invalid")
					os.Exit(2)
				}
				options.Calls = manager
				go func() {
					if runErr := manager.Run(ctx); runErr != nil && ctx.Err() == nil {
						logger.Error("ARI coordinator stopped")
						stop()
					}
				}()
			}
		}
	} else if sipCompatibility && strings.TrimSpace(os.Getenv("ARI_URL")) != "" {
		logger.Error("SECRETS_ENCRYPTION_KEY is required when ARI is enabled")
		os.Exit(2)
	}
	options.SIP.Disabled = !sipCompatibility
	api := httpapi.NewWithOptions(database, logger, options)
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
