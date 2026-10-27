package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/push"
)

const (
	pushPeriod      = time.Second
	pushSendTimeout = 10 * time.Second
	pushMaxBackoff  = 5 * time.Minute
	pushBatchLimit  = 100
)

type pushEndpointRequest struct {
	Endpoint string `json:"endpoint"`
}

// pushReady reports whether this node posts wake-ups to registered endpoints.
// It needs no credentials: the phone's endpoint (usually a Kyubey relay)
// holds them.
func (s *Server) pushReady() bool {
	return s.push != nil
}

// putPushEndpoint registers the calling phone's push endpoint after it has
// accepted a "check" request, so only endpoints that reach the phone are kept.
func (s *Server) putPushEndpoint(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	if !s.pushReady() {
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "This server does not send push wake-ups.", false)
		return
	}
	var request pushEndpointRequest
	if !decodeOrError(w, r, &request, push.MaxEndpointLength+256) {
		return
	}
	if err := s.push.Validate(request.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PUSH_ENDPOINT", "The push endpoint must be a public https:// URL.", false)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushSendTimeout)
	err := s.push.Send(ctx, request.Endpoint, "check", principal.DeviceID)
	cancel()
	switch {
	case errors.Is(err, push.ErrInvalidEndpoint):
		writeError(w, http.StatusBadRequest, "INVALID_PUSH_ENDPOINT", "The push endpoint must be a public https:// URL.", false)
		return
	case errors.Is(err, push.ErrEndpointGone), errors.Is(err, push.ErrEndpointRefused):
		writeError(w, http.StatusUnprocessableEntity, "PUSH_ENDPOINT_REJECTED", "The push endpoint does not reach this device.", false)
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "The push endpoint is temporarily unavailable.", true)
		return
	}
	// Start from the current events: registering must not announce history.
	// A different endpoint is a new generation, so answers about the old one
	// no longer touch it.
	var generation int64
	if err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO push_endpoints(device_id,endpoint,pushed_event_cursor,pushed_call_cursor,delivered_at)
		VALUES ($1,$2,
			COALESCE((SELECT MAX(cursor) FROM server_events WHERE owner_id=$3),0),
			COALESCE((SELECT MAX(cursor) FROM call_events WHERE client_device_id=$1),0),
			now())
		ON CONFLICT (device_id) DO UPDATE SET endpoint=EXCLUDED.endpoint,
			generation=push_endpoints.generation+CASE WHEN push_endpoints.endpoint=EXCLUDED.endpoint THEN 0 ELSE 1 END,
			pushed_event_cursor=EXCLUDED.pushed_event_cursor, pushed_call_cursor=EXCLUDED.pushed_call_cursor,
			failures=0, call_failures=0, next_attempt_at=now(), updated_at=now(), delivered_at=now()
		RETURNING generation`,
		principal.DeviceID, request.Endpoint, principal.OwnerID).Scan(&generation); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"push": "ready", "endpoint_generation": generation})
}

func (s *Server) deletePushEndpoint(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM push_endpoints WHERE device_id=$1`, principal.DeviceID); err != nil {
		writeDBUnavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pushBackoff(failures int) time.Duration {
	if failures > 9 {
		return pushMaxBackoff
	}
	delay := time.Second << failures
	if delay > pushMaxBackoff {
		return pushMaxBackoff
	}
	return delay
}

// pushTestEvery bounds how often a device may ask for a test wake-up.
const pushTestEvery = time.Minute

// getPushEndpoint reports how this node's wake-ups for the calling device
// fare at its push endpoint: whether one is registered, its generation,
// consecutive failed hand-offs (SMS and wire 1, and calls apart), and when
// the relay last accepted one. The endpoint itself is never returned.
func (s *Server) getPushEndpoint(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	out, err := scanPushStatus(s.db.QueryRowContext(r.Context(), `SELECT failures, call_failures, generation, delivered_at FROM push_endpoints WHERE device_id=$1`,
		principal.DeviceID))
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"registered": false})
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// testPushEndpoint sends the calling device a silent wake-up now, so it can
// tell whether this node's pushes reach it. It is an "sms" hint: the relay
// only validates "check" hints without delivering them, and the phone answers
// an sms hint with one sync that shows nothing when nothing is new. The
// answer says whether the relay accepted it; whether the phone received it
// only the phone knows.
func (s *Server) testPushEndpoint(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	if !s.pushReady() {
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "This server does not send push wake-ups.", false)
		return
	}
	var endpoint string
	var recent bool
	err := s.db.QueryRowContext(r.Context(), `SELECT endpoint, COALESCE(tested_at>now()-make_interval(secs=>$2::int), false)
      FROM push_endpoints WHERE device_id=$1`, principal.DeviceID, int(pushTestEvery.Seconds())).Scan(&endpoint, &recent)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "PUSH_NOT_REGISTERED", "This device has no push endpoint on this node.", false)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if recent {
		writeError(w, http.StatusTooManyRequests, "PUSH_TEST_TOO_SOON", "A test wake-up was sent less than a minute ago.", true)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `UPDATE push_endpoints SET tested_at=now() WHERE device_id=$1`, principal.DeviceID); err != nil {
		writeDBUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushSendTimeout)
	err = s.push.Send(ctx, endpoint, "sms", principal.DeviceID)
	cancel()
	if err != nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE push_endpoints SET failures=failures+1 WHERE device_id=$1 AND endpoint=$2`, principal.DeviceID, endpoint)
		writeJSON(w, http.StatusOK, map[string]any{"sent": false})
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `UPDATE push_endpoints SET failures=0, delivered_at=now() WHERE device_id=$1 AND endpoint=$2`, principal.DeviceID, endpoint)
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}
