package rtcsignal

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Authenticate must verify the bearer session on every call, not merely reuse
// the handshake principal. It is called on connect, each command and each pulse.
// The caller must also supply a repository with a production Authorizer before
// mounting this handler. Nothing in cmd/api or /v1/ws mounts it currently.
type Authenticate func(*http.Request) (Principal, error)

type WebSocketOptions struct {
	Service      *Service
	Authenticate Authenticate
	// Limits belong exclusively to calling, not the SMS wake connection budget.
	MaxConnections          int
	MaxConnectionsPerDevice int
	QueueDepth              int
	AuthCheckInterval       time.Duration
}

type WebSocketHandler struct {
	options     WebSocketOptions
	mu          sync.Mutex
	connections map[*signalConnection]struct{}
	closing     bool
}

type signalConnection struct {
	owner        *WebSocketHandler
	ws           *websocket.Conn
	request      *http.Request
	principal    Principal
	legID        string
	queue        chan any
	done         chan struct{}
	once         sync.Once
	snapshotMu   sync.Mutex
	lastRevision int64
}

type signalMessage struct {
	Type      string      `json:"type"`
	State     *ReadResult `json:"state,omitempty"`
	Ack       *Ack        `json:"ack,omitempty"`
	Duplicate bool        `json:"duplicate,omitempty"`
	Code      string      `json:"code,omitempty"`
}

func NewWebSocketHandler(options WebSocketOptions) (*WebSocketHandler, error) {
	if options.Service == nil || options.Authenticate == nil {
		return nil, ErrInvalid
	}
	if options.MaxConnections == 0 {
		options.MaxConnections = 64
	}
	if options.MaxConnectionsPerDevice == 0 {
		options.MaxConnectionsPerDevice = 2
	}
	if options.QueueDepth == 0 {
		options.QueueDepth = 16
	}
	if options.AuthCheckInterval == 0 {
		options.AuthCheckInterval = 10 * time.Second
	}
	if options.MaxConnections < 1 || options.MaxConnections > 1024 || options.MaxConnectionsPerDevice < 1 || options.MaxConnectionsPerDevice > 4 || options.QueueDepth < 1 || options.QueueDepth > 64 || options.AuthCheckInterval <= 0 || options.AuthCheckInterval > 15*time.Second {
		return nil, ErrInvalid
	}
	return &WebSocketHandler{options: options, connections: map[*signalConnection]struct{}{}}, nil
}

func (h *WebSocketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.TLS == nil {
		http.Error(w, "RTC signaling requires HTTPS GET", http.StatusBadRequest)
		return
	}
	// Native clients do not send Origin. Browser/cookie-based use is not part of
	// this contract, and cross-origin upgrades are rejected before allocating.
	if len(r.Header.Values("Origin")) > 0 {
		http.Error(w, "RTC origin is not allowed", http.StatusForbidden)
		return
	}
	legID := r.PathValue("leg_id")
	if !validID(legID) {
		http.Error(w, "Invalid RTC leg", http.StatusBadRequest)
		return
	}
	c := &signalConnection{owner: h, request: r, legID: legID, queue: make(chan any, h.options.QueueDepth), done: make(chan struct{})}
	h.mu.Lock()
	if h.closing || len(h.connections) >= h.options.MaxConnections {
		h.mu.Unlock()
		http.Error(w, "RTC connection budget exceeded", http.StatusTooManyRequests)
		return
	}
	// Reserve before authentication/storage work so concurrent handshakes,
	// including invalid ones, cannot exceed the voice admission budget.
	h.connections[c] = struct{}{}
	h.mu.Unlock()
	defer func() { c.close(websocket.CloseNormalClosure); h.mu.Lock(); delete(h.connections, c); h.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	p, err := h.options.Authenticate(r.WithContext(ctx))
	cancel()
	if err != nil {
		http.Error(w, "RTC authentication required", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	count := 0
	for existing := range h.connections {
		if existing != c && existing.principal == p {
			count++
		}
	}
	if h.closing || count >= h.options.MaxConnectionsPerDevice {
		h.mu.Unlock()
		http.Error(w, "RTC connection budget exceeded", http.StatusTooManyRequests)
		return
	}
	c.principal = p
	h.mu.Unlock()
	ctx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
	state, err := h.options.Service.Read(ctx, p, legID)
	cancel()
	if err != nil {
		http.Error(w, "RTC leg unavailable", http.StatusForbidden)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 3 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	c.ws = ws
	closing := h.closing
	h.mu.Unlock()
	if closing {
		ws.Close()
		return
	}
	ws.SetReadLimit(MaxFrameBytes)
	ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	go c.writeLoop()
	c.snapshot(state)
	// Per-connection token bucket: two permitted connections cannot grow without
	// bound. A flood closes this voice connection; it never stops control sync.
	tokens := 16.0
	last := time.Now()
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		now := time.Now()
		tokens += now.Sub(last).Seconds() * 8
		if tokens > 16 {
			tokens = 16
		}
		last = now
		if tokens < 1 {
			c.close(websocket.ClosePolicyViolation)
			return
		}
		tokens--
		if kind != websocket.TextMessage {
			c.close(websocket.CloseUnsupportedData)
			return
		}
		if !c.authorized() {
			return
		}
		command, err := DecodeCommand(data)
		if err != nil || command.LegID != legID {
			c.send(signalMessage{Type: "error", Code: "INVALID_SIGNAL"})
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		ack, duplicate, err := h.options.Service.Apply(ctx, p, command)
		cancel()
		if err != nil {
			code := "SIGNAL_UNAVAILABLE"
			switch {
			case errors.Is(err, ErrForbidden):
				c.close(websocket.ClosePolicyViolation)
				return
			case errors.Is(err, ErrConflict):
				code = "STATE_CONFLICT"
			case errors.Is(err, ErrClosed):
				code = "LEG_CLOSED"
			case errors.Is(err, ErrExpired):
				code = "AUTHORIZATION_EXPIRED"
			case errors.Is(err, ErrLimit):
				code = "SIGNAL_BUDGET_EXCEEDED"
			}
			c.send(signalMessage{Type: "error", Code: code})
			continue
		}
		c.send(signalMessage{Type: "ack", Ack: &ack, Duplicate: duplicate})
		h.broadcast(legID)
	}
}

func (c *signalConnection) authorized() bool {
	ctx, cancel := context.WithTimeout(c.request.Context(), 3*time.Second)
	defer cancel()
	p, err := c.owner.options.Authenticate(c.request.WithContext(ctx))
	if err != nil || p != c.principal {
		c.close(websocket.ClosePolicyViolation)
		return false
	}
	return true
}
func (c *signalConnection) send(value any) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.queue <- value:
	default:
		c.close(websocket.CloseTryAgainLater)
	}
}
func (c *signalConnection) snapshot(state ReadResult) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if state.Snapshot.Revision < c.lastRevision {
		return
	}
	c.lastRevision = state.Snapshot.Revision
	c.send(signalMessage{Type: "snapshot", State: &state})
}
func (c *signalConnection) close(code int) {
	c.once.Do(func() {
		close(c.done)
		c.owner.mu.Lock()
		ws := c.ws
		c.owner.mu.Unlock()
		if ws != nil {
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
			ws.Close()
		}
	})
}
func (c *signalConnection) writeLoop() {
	ticker := time.NewTicker(c.owner.options.AuthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case value := <-c.queue:
			c.ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if c.ws.WriteJSON(value) != nil {
				c.close(websocket.CloseGoingAway)
				return
			}
		case <-ticker.C:
			if !c.authorized() {
				return
			}
			ctx, cancel := context.WithTimeout(c.request.Context(), 3*time.Second)
			state, err := c.owner.options.Service.Read(ctx, c.principal, c.legID)
			cancel()
			if err != nil {
				c.close(websocket.ClosePolicyViolation)
				return
			}
			c.snapshot(state)
			if c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
				c.close(websocket.CloseGoingAway)
				return
			}
		}
	}
}
func (h *WebSocketHandler) broadcast(legID string) {
	h.mu.Lock()
	var targets []*signalConnection
	for c := range h.connections {
		if c.ws != nil && c.legID == legID {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		if !c.authorized() {
			continue
		}
		ctx, cancel := context.WithTimeout(c.request.Context(), 3*time.Second)
		state, err := h.options.Service.Read(ctx, c.principal, c.legID)
		cancel()
		if err != nil {
			c.close(websocket.ClosePolicyViolation)
			continue
		}
		c.snapshot(state)
	}
}

// Close drains only this isolated call-signaling handler, never SMS wake peers.
func (h *WebSocketHandler) Close() {
	h.mu.Lock()
	h.closing = true
	var connections []*signalConnection
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.close(websocket.CloseGoingAway)
	}
}
