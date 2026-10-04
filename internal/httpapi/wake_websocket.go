package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxWakeConnectionsPerDevice = 2
	wakeReadLimit               = 4 * 1024
	wakePingPeriod              = 20 * time.Second
	wakePongWait                = 45 * time.Second
	wakeWriteWait               = 5 * time.Second
	wakeQueryTimeout            = 2 * time.Second
	wakePollPeriod              = time.Second
)

var errWakeApplicationFrame = errors.New("client application frames are not supported")

type wakeMarker struct {
	cursor        int64
	callCursor    int64
	count         int64
	latestUpdated sql.NullTime
	latestCreated sql.NullTime
	nextExpiry    sql.NullTime
}

var wakeUpgrader = websocket.Upgrader{
	HandshakeTimeout: 5 * time.Second,
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	// Android's OkHttp client does not send Origin. Browser clients may connect
	// only from the request's own origin; cross-origin cookies are not used.
	CheckOrigin: sameWakeOrigin,
}

func (s *Server) wakeWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "The wake channel does not accept query parameters.", false)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "The wake channel does not accept a request body.", false)
		return
	}
	if !sameWakeOrigin(r) {
		writeError(w, http.StatusForbidden, "ORIGIN_FORBIDDEN", "The WebSocket origin is not allowed.", false)
		return
	}

	authCtx, cancel := context.WithTimeout(r.Context(), wakeQueryTimeout)
	principal, err := s.authenticate(r.WithContext(authCtx))
	cancel()
	if err != nil {
		if errors.Is(err, errInvalidCredentials) || errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Authentication is required.", false)
		} else {
			writeDBUnavailable(w)
		}
		return
	}
	if principal.Role != "client" && principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "This device role cannot use the wake channel.", false)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, "WEBSOCKET_REQUIRED", "A WebSocket upgrade is required.", false)
		return
	}
	if !s.reserveWakeConnection(principal.DeviceID) {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "This device has reached its WebSocket connection limit.", true)
		return
	}
	defer s.releaseWakeConnection(principal.DeviceID)

	conn, err := wakeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if !s.registerWakeConnection(conn) {
		writeWakeClose(conn, websocket.CloseGoingAway, "server shutting down")
		_ = conn.Close()
		return
	}
	defer func() {
		_ = conn.Close()
		s.unregisterWakeConnection(conn)
	}()

	conn.SetReadLimit(wakeReadLimit)
	if err := conn.SetReadDeadline(time.Now().Add(wakePongWait)); err != nil {
		writeWakeClose(conn, websocket.CloseInternalServerErr, "connection setup failed")
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wakePongWait))
	})

	marker, err := s.readWakeMarker(r.Context(), principal)
	if err != nil {
		writeWakeClose(conn, websocket.CloseInternalServerErr, "wake state unavailable")
		return
	}
	if err := writeWakeHint(conn); err != nil {
		return
	}

	readResults := make(chan error, 1)
	go func() {
		for {
			messageType, _, readErr := conn.ReadMessage()
			if readErr != nil {
				readResults <- readErr
				return
			}
			if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
				readResults <- errWakeApplicationFrame
				return
			}
		}
	}()

	pollTicker := time.NewTicker(wakePollPeriod)
	defer pollTicker.Stop()
	pingTicker := time.NewTicker(wakePingPeriod)
	defer pingTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case readErr := <-readResults:
			if errors.Is(readErr, errWakeApplicationFrame) {
				writeWakeClose(conn, websocket.ClosePolicyViolation, "client messages are not supported")
			} else if errors.Is(readErr, websocket.ErrReadLimit) {
				// Gorilla already sent the required 1009 close frame.
			} else if !isWebSocketPeerClose(readErr) {
				// The read deadline, network failure, and malformed frames all end
				// this connection. HTTPS remains the durable synchronization path.
			}
			return
		case <-pollTicker.C:
			pollCtx, pollCancel := context.WithTimeout(r.Context(), wakeQueryTimeout)
			current, authErr := s.authenticate(r.WithContext(pollCtx))
			if errors.Is(authErr, errInvalidCredentials) || errors.Is(authErr, sql.ErrNoRows) ||
				(authErr == nil && !sameWakePrincipal(principal, current)) {
				pollCancel()
				writeWakeClose(conn, websocket.ClosePolicyViolation, "session expired or revoked")
				return
			}
			if authErr != nil {
				pollCancel()
				writeWakeClose(conn, websocket.CloseInternalServerErr, "session check unavailable")
				return
			}
			currentMarker, markerErr := s.readWakeMarker(pollCtx, principal)
			pollCancel()
			if markerErr != nil {
				writeWakeClose(conn, websocket.CloseInternalServerErr, "wake state unavailable")
				return
			}
			if !sameWakeMarker(marker, currentMarker) {
				if err := writeWakeHint(conn); err != nil {
					return
				}
				marker = currentMarker
			}
		case <-pingTicker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wakeWriteWait)); err != nil {
				return
			}
		}
	}
}

func sameWakeOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.Host == "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
		return false
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

func (s *Server) reserveWakeConnection(deviceID string) bool {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsClosing || s.wsByDevice[deviceID] >= maxWakeConnectionsPerDevice {
		return false
	}
	s.wsByDevice[deviceID]++
	return true
}

func (s *Server) releaseWakeConnection(deviceID string) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsByDevice[deviceID] <= 1 {
		delete(s.wsByDevice, deviceID)
	} else {
		s.wsByDevice[deviceID]--
	}
}

func (s *Server) registerWakeConnection(conn *websocket.Conn) bool {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsClosing {
		return false
	}
	s.wsConnections[conn] = struct{}{}
	s.wsHandlers.Add(1)
	return true
}

func (s *Server) unregisterWakeConnection(conn *websocket.Conn) {
	s.wsMu.Lock()
	delete(s.wsConnections, conn)
	s.wsMu.Unlock()
	s.wsHandlers.Done()
}

// ShutdownWebSockets closes hijacked connections, which net/http.Server.Shutdown
// does not manage. Call it before shutting down the HTTP server.
func (s *Server) ShutdownWebSockets() {
	s.wsMu.Lock()
	s.wsClosing = true
	connections := make([]*websocket.Conn, 0, len(s.wsConnections))
	for conn := range s.wsConnections {
		connections = append(connections, conn)
	}
	s.wsMu.Unlock()

	for _, conn := range connections {
		writeWakeClose(conn, websocket.CloseGoingAway, "server shutting down")
		_ = conn.Close()
	}
	s.wsHandlers.Wait()
}

func (s *Server) readWakeMarker(parent context.Context, principal Principal) (wakeMarker, error) {
	ctx, cancel := context.WithTimeout(parent, wakeQueryTimeout)
	defer cancel()
	var marker wakeMarker
	switch principal.Role {
	case "client":
		err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(cursor),0) FROM server_events WHERE owner_id=$1`, principal.OwnerID).Scan(&marker.cursor)
		if err != nil {
			return wakeMarker{}, err
		}
		err = s.db.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(cursor),0) FROM call_events
			WHERE owner_id=$1 AND client_device_id=$2`, principal.OwnerID, principal.DeviceID).Scan(&marker.callCursor)
		return marker, err
	case "gateway":
		err := s.db.QueryRowContext(ctx, `
			SELECT count(*), max(updated_at), max(created_at), min(expires_at)
			FROM commands
			WHERE gateway_id=$1 AND owner_id=$2
			  AND ((status='queued' AND expires_at>now()) OR status IN ('accepted_by_gateway','dispatching'))`,
			principal.DeviceID, principal.OwnerID).Scan(
			&marker.count, &marker.latestUpdated, &marker.latestCreated, &marker.nextExpiry)
		return marker, err
	default:
		return wakeMarker{}, errInvalidCredentials
	}
}

func sameWakePrincipal(expected, current Principal) bool {
	return expected.DeviceID == current.DeviceID && expected.OwnerID == current.OwnerID && expected.Role == current.Role
}

func sameWakeMarker(a, b wakeMarker) bool {
	return a.cursor == b.cursor && a.callCursor == b.callCursor && a.count == b.count &&
		sameWakeTime(a.latestUpdated, b.latestUpdated) &&
		sameWakeTime(a.latestCreated, b.latestCreated) &&
		sameWakeTime(a.nextExpiry, b.nextExpiry)
}

func sameWakeTime(a, b sql.NullTime) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Time.Equal(b.Time)
}

func writeWakeHint(conn *websocket.Conn) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wakeWriteWait)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol_version":1,"type":"sync_required"}`))
}

func writeWakeClose(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(wakeWriteWait))
}

func isWebSocketPeerClose(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr)
}
