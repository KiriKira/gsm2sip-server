package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/calls"
	"github.com/kirikira/gsm2sip-server/internal/securestore"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	apiPrefix       = "/v1"
	accessLifetime  = 15 * time.Minute
	refreshLifetime = 30 * 24 * time.Hour
	maxJSONBody     = 256 * 1024
)

type Server struct {
	db           *sql.DB
	logger       *slog.Logger
	now          func() time.Time
	secretCipher *securestore.Cipher
	calls        *calls.Manager
	sip          SIPSettings

	wsMu          sync.Mutex
	wsByDevice    map[string]int
	wsConnections map[*websocket.Conn]struct{}
	wsClosing     bool
	wsHandlers    sync.WaitGroup
}

func New(database *sql.DB, logger *slog.Logger) *Server {
	return NewWithOptions(database, logger, Options{})
}

type Options struct {
	SecretCipher *securestore.Cipher
	Calls        *calls.Manager
	SIP          SIPSettings
}

func NewWithOptions(database *sql.DB, logger *slog.Logger, options Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		db: database, logger: logger, now: time.Now,
		secretCipher: options.SecretCipher, calls: options.Calls, sip: options.SIP,
		wsByDevice: make(map[string]int), wsConnections: make(map[*websocket.Conn]struct{}),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/pairings/claim", s.claimPairing)
	mux.HandleFunc("POST /v1/auth/refresh", s.refreshSession)
	mux.HandleFunc("POST /v1/auth/revoke", s.revokeSession)
	mux.HandleFunc("GET /v1/ws", s.wakeWebSocket)
	mux.HandleFunc("GET /v1/devices/self/sip-config", s.authenticated(s.getSIPConfig))
	mux.HandleFunc("POST /v1/devices/self/sip-credentials/rotate", s.authenticated(s.rotateSIPCredentials))
	mux.HandleFunc("GET /v1/clients", s.authenticated(s.listClients))
	mux.HandleFunc("GET /v1/gateways", s.authenticated(s.listGateways))
	mux.HandleFunc("GET /v1/gateways/{gateway_id}/sims", s.authenticated(s.listSIMs))
	mux.HandleFunc("POST /v1/gateways/{gateway_id}/heartbeat", s.authenticated(s.heartbeat))
	mux.HandleFunc("POST /v1/gateways/{gateway_id}/sim-bindings", s.authenticated(s.updateSIMBindings))
	mux.HandleFunc("GET /v1/gateways/{gateway_id}/commands", s.authenticated(s.listCommands))
	mux.HandleFunc("POST /v1/gateways/{gateway_id}/commands/{command_id}/claim", s.authenticated(s.claimCommand))
	mux.HandleFunc("POST /v1/gateways/{gateway_id}/events:batch", s.authenticated(s.uploadEvents))
	mux.HandleFunc("GET /v1/messages", s.authenticated(s.listMessages))
	mux.HandleFunc("POST /v1/messages", s.authenticated(s.createMessage))
	mux.HandleFunc("GET /v1/messages/{message_id}", s.authenticated(s.getMessage))
	mux.HandleFunc("GET /v1/events", s.authenticated(s.listEvents))
	mux.HandleFunc("POST /v1/events/ack", s.authenticated(s.acknowledgeClientEvents))
	mux.HandleFunc("GET /v1/calls", s.authenticated(s.listCalls))
	mux.HandleFunc("GET /v1/calls/{call_id}", s.authenticated(s.getCall))
	mux.HandleFunc("POST /v1/call-intents", s.authenticated(s.createCallIntent))
	mux.HandleFunc("DELETE /v1/call-intents/{intent_id}", s.authenticated(s.cancelCallIntent))
	mux.HandleFunc("POST /v1/clients/{client_id}/ready", s.authenticated(s.clientReady))
	return requestIDMiddleware(recoverMiddleware(s.logger, mux))
}

type principalKey struct{}

var errInvalidCredentials = errors.New("invalid credentials")

type authedHandler func(http.ResponseWriter, *http.Request, Principal)

func (s *Server) authenticated(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticate(r)
		if err != nil {
			if errors.Is(err, errInvalidCredentials) || errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Authentication is required.", false)
			} else {
				writeDBUnavailable(w)
			}
			return
		}
		next(w, r, principal)
	}
}

func (s *Server) authenticate(r *http.Request) (Principal, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) <= len("Bearer ") {
		return Principal{}, errInvalidCredentials
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return Principal{}, errInvalidCredentials
	}
	hash := tokenHash(token)
	var p Principal
	err := s.db.QueryRowContext(r.Context(), `
		SELECT d.id::text, d.owner_id::text, d.role, d.name
		FROM sessions s
		JOIN devices d ON d.id=s.device_id
		WHERE s.access_hash=$1 AND s.access_expires_at > now() AND d.state='active'`, hash).
		Scan(&p.DeviceID, &p.OwnerID, &p.Role, &p.Name)
	return p, err
}

func principalFrom(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	return p
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "PostgreSQL is not ready.", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) notReady(code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, code, "SIP and ARI calling are not configured.", false)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	body := errorBody{RequestID: requestID(w)}
	body.Error.Code, body.Error.Message, body.Error.Retryable = code, message, retryable
	writeJSON(w, status, body)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if limit <= 0 || limit > maxJSONBody {
		limit = maxJSONBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func decodeOrError(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	err := decodeJSON(w, r, dst, limit)
	if err == nil {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body exceeds the protocol limit.", false)
		return false
	}
	writeError(w, http.StatusBadRequest, "INVALID_JSON", "Request body is not valid for this endpoint.", false)
	return false
}

func beginDurable(ctx context.Context, database *sql.DB) (*sql.Tx, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newUUID()
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type requestIDKey struct{}

func requestID(w http.ResponseWriter) string {
	if value := w.Header().Get("X-Request-ID"); value != "" {
		return value
	}
	return newUUID()
}

func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request panic", "request_id", w.Header().Get("X-Request-ID"), "error", fmt.Sprint(value))
				writeError(w, http.StatusInternalServerError, "INTERNAL", "The request could not be completed.", true)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
