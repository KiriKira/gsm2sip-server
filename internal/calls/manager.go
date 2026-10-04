package calls

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

const (
	intentLifetime       = 30 * time.Second
	incomingCallLifetime = 25 * time.Second
	ariReconcilePeriod   = 5 * time.Second
	ariReadyLifetime     = 15 * time.Second
)

type Manager struct {
	db        *sql.DB
	config    Config
	ari       *asterisk.Client
	configErr error

	mu         sync.RWMutex
	bridgeMu   sync.Mutex
	ariHealthy bool
	ariReadyAt time.Time
	connection *websocket.Conn
}

func NewManager(database *sql.DB, config Config) (*Manager, error) {
	if database == nil {
		return nil, errors.New("call manager requires a database")
	}
	m := &Manager{db: database, config: config}
	ariFields := []string{config.ARIURL, config.ARIUsername, config.ARIPassword, config.ARIApplication, config.SIPRealm}
	configuredCount := 0
	for _, field := range ariFields {
		if strings.TrimSpace(field) != "" {
			configuredCount++
		}
	}
	if configuredCount == 0 {
		m.configErr = ErrNotConfigured
		return m, nil
	}
	if configuredCount != len(ariFields) {
		return nil, errors.New("ARI URL, username, password, application, and SIP realm must be configured together")
	}
	if len(config.TokenKey) < 32 {
		return nil, errors.New("call token key must contain at least 32 bytes")
	}
	if !validSIPRealm(config.SIPRealm) {
		return nil, errors.New("SIP realm must be a DNS name or IP address without a scheme or port")
	}
	client, err := asterisk.New(asterisk.Config{
		BaseURL: config.ARIURL, Username: config.ARIUsername, Password: config.ARIPassword, App: config.ARIApplication,
	})
	if err != nil {
		return nil, err
	}
	m.ari = client
	return m, nil
}

func validSIPRealm(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "@/:?# \t\r\n") {
		return false
	}
	if ip := net.ParseIP(value); ip != nil {
		return true
	}
	host := strings.TrimSuffix(value, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return true
}

func (m *Manager) Configured() bool { return m != nil && m.configErr == nil && m.ari != nil }

// Ready reports whether the authenticated ARI event stream has been connected,
// channel reconciliation succeeded, and that connection has remained fresh.
func (m *Manager) Ready(ctx context.Context) bool {
	if !m.Configured() {
		return false
	}
	m.mu.RLock()
	ready := m.ariHealthy && time.Since(m.ariReadyAt) <= ariReadyLifetime
	m.mu.RUnlock()
	if !ready {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return m.ari.Check(checkCtx) == nil
}

// Run is the single-process ARI/Stasis coordinator loop. It reconnects after
// transport failure and reconciles active channels before admitting new work.
func (m *Manager) Run(ctx context.Context) error {
	if !m.Configured() {
		return ErrNotConfigured
	}
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := m.ari.Events(ctx)
		if err != nil {
			slog.Error("ARI event connection failed", "error", err)
			if !waitContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		m.mu.Lock()
		m.connection = conn
		m.ariHealthy = false
		m.mu.Unlock()
		backoff = time.Second
		startupErr := m.ari.Check(ctx)
		if startupErr == nil {
			// A coordinator process may have restarted while Asterisk was still
			// carrying a call. The new event stream has no history for that call,
			// so fail closed before accepting new work.
			startupErr = m.failClosed(ctx, "coordinator_restarted")
		}
		if startupErr == nil {
			startupErr = m.reconcile(ctx)
		}
		if startupErr != nil {
			slog.Error("ARI initial reconciliation failed", "error", startupErr)
			_ = conn.Close()
			m.markDisconnected(conn)
			if !waitContext(ctx, backoff) {
				return ctx.Err()
			}
			continue
		}
		m.markReady(conn)
		if err := m.runConnection(ctx, conn); err != nil && ctx.Err() == nil {
			slog.Warn("ARI event stream disconnected", "error", err)
		}
		m.markDisconnected(conn)
		_ = conn.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		if err := m.failClosed(cleanupCtx, "ari_event_stream_disconnected"); err != nil {
			slog.Warn("active calls could not be fully stopped after ARI disconnect", "error", err)
		}
		cancel()
		if !waitContext(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return ctx.Err()
}

func (m *Manager) runConnection(ctx context.Context, conn *websocket.Conn) error {
	events := make(chan asterisk.Event, 64)
	readErr := make(chan error, 1)
	go func() {
		for {
			var event asterisk.Event
			if err := conn.ReadJSON(&event); err != nil {
				readErr <- err
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	reconcileTicker := time.NewTicker(ariReconcilePeriod)
	defer reconcileTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case event := <-events:
			if err := m.handleEvent(ctx, event); err != nil {
				slog.Error("ARI event could not be reconciled", "event", event.Type, "error", err)
				return err
			}
			m.refreshReady(conn)
		case <-reconcileTicker.C:
			if err := m.reconcile(ctx); err != nil {
				m.markNotReady(conn)
				return err
			}
			m.refreshReady(conn)
		}
	}
}

func (m *Manager) markReady(conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == conn {
		m.ariHealthy = true
		m.ariReadyAt = time.Now()
	}
}

func (m *Manager) refreshReady(conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == conn && m.ariHealthy {
		m.ariReadyAt = time.Now()
	}
}

func (m *Manager) markNotReady(conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == conn {
		m.ariHealthy = false
	}
}

func (m *Manager) markDisconnected(conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == conn {
		m.connection = nil
		m.ariHealthy = false
		m.ariReadyAt = time.Time{}
	}
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) tokenFor(intentID string) string {
	mac := hmac.New(sha256.New, m.config.TokenKey)
	_, _ = mac.Write([]byte("gsm2sip/call-intent/v1\x00" + strings.ToLower(intentID)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *Manager) idempotencyHash(ownerID, clientID, key string) []byte {
	mac := hmac.New(sha256.New, m.config.TokenKey)
	_, _ = mac.Write([]byte("gsm2sip/call-idempotency/v1\x00" + strings.ToLower(ownerID) + "\x00" + strings.ToLower(clientID) + "\x00" + key))
	return mac.Sum(nil)
}

func (m *Manager) makeWakeNonce(callID string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func hashPayload(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func equalBytes(a, b []byte) bool { return hmac.Equal(a, b) }
