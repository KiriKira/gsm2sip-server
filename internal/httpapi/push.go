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
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO push_endpoints(device_id,endpoint,pushed_event_cursor,pushed_call_cursor,delivered_at)
		VALUES ($1,$2,
			COALESCE((SELECT MAX(cursor) FROM server_events WHERE owner_id=$3),0),
			COALESCE((SELECT MAX(cursor) FROM call_events WHERE client_device_id=$1),0),
			now())
		ON CONFLICT (device_id) DO UPDATE SET endpoint=EXCLUDED.endpoint,
			pushed_event_cursor=EXCLUDED.pushed_event_cursor, pushed_call_cursor=EXCLUDED.pushed_call_cursor,
			failures=0, next_attempt_at=now(), updated_at=now(), delivered_at=now()`,
		principal.DeviceID, request.Endpoint, principal.OwnerID); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"push": "ready"})
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

// runPush announces new SMS and call events to registered phones. Each push
// covers every event since the previous one; failures back off per device.
func (s *Server) runPush(ctx context.Context) {
	if s.push == nil {
		return
	}
	tick := time.NewTicker(pushPeriod)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.pushPending(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("push dispatch failed")
		}
	}
}

type pendingPush struct {
	deviceID                string
	endpoint                string
	eventCursor, callCursor int64
	pushedCallCursor        int64
	failures                int
}

func (s *Server) pushPending(ctx context.Context) error {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	rows, err := s.db.QueryContext(queryCtx, `
		SELECT device_id::text, endpoint, event_cursor, call_cursor, pushed_call_cursor, failures FROM (
			SELECT t.device_id, t.endpoint, t.failures, t.pushed_event_cursor, t.pushed_call_cursor,
				COALESCE((SELECT MAX(e.cursor) FROM server_events e WHERE e.owner_id=d.owner_id),0) AS event_cursor,
				COALESCE((SELECT MAX(c.cursor) FROM call_events c WHERE c.client_device_id=t.device_id),0) AS call_cursor
			FROM push_endpoints t JOIN devices d ON d.id=t.device_id
			WHERE d.state='active' AND t.next_attempt_at<=now()
		) pending
		WHERE event_cursor>pushed_event_cursor OR call_cursor>pushed_call_cursor
		LIMIT $1`, pushBatchLimit)
	if err != nil {
		cancel()
		return err
	}
	var batch []pendingPush
	for rows.Next() {
		var item pendingPush
		if err := rows.Scan(&item.deviceID, &item.endpoint, &item.eventCursor, &item.callCursor, &item.pushedCallCursor, &item.failures); err != nil {
			rows.Close()
			cancel()
			return err
		}
		batch = append(batch, item)
	}
	err = rows.Err()
	rows.Close()
	cancel()
	if err != nil {
		return err
	}
	for _, item := range batch {
		kind := "sms"
		if item.callCursor > item.pushedCallCursor {
			kind = "call"
		}
		sendCtx, stop := context.WithTimeout(ctx, pushSendTimeout)
		sendErr := s.push.Send(sendCtx, item.endpoint, kind, item.deviceID)
		stop()
		if ctx.Err() != nil {
			return nil
		}
		var execErr error
		switch {
		case sendErr == nil:
			_, execErr = s.db.ExecContext(ctx, `
				UPDATE push_endpoints SET pushed_event_cursor=GREATEST(pushed_event_cursor,$2),
					pushed_call_cursor=GREATEST(pushed_call_cursor,$3), failures=0, delivered_at=now()
				WHERE device_id=$1 AND endpoint=$4`, item.deviceID, item.eventCursor, item.callCursor, item.endpoint)
		case errors.Is(sendErr, push.ErrEndpointGone), errors.Is(sendErr, push.ErrEndpointRefused), errors.Is(sendErr, push.ErrInvalidEndpoint):
			// The phone registers a working endpoint again on its next check.
			_, execErr = s.db.ExecContext(ctx, `DELETE FROM push_endpoints WHERE device_id=$1 AND endpoint=$2`, item.deviceID, item.endpoint)
		default:
			delay := pushBackoff(item.failures + 1)
			_, execErr = s.db.ExecContext(ctx, `
				UPDATE push_endpoints SET failures=failures+1, next_attempt_at=now()+$2*interval '1 millisecond'
				WHERE device_id=$1 AND endpoint=$3`, item.deviceID, delay.Milliseconds(), item.endpoint)
		}
		if execErr != nil {
			return execErr
		}
	}
	return nil
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
// fare at its push endpoint: whether one is registered, consecutive failed
// hand-offs, and when the relay last accepted one. The endpoint itself is
// never returned.
func (s *Server) getPushEndpoint(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	var failures int
	var delivered sql.NullTime
	err := s.db.QueryRowContext(r.Context(), `SELECT failures, delivered_at FROM push_endpoints WHERE device_id=$1`,
		principal.DeviceID).Scan(&failures, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"registered": false})
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	out := map[string]any{"registered": true, "failures": failures}
	if delivered.Valid {
		out["delivered_at"] = delivered.Time.UTC().Format(time.RFC3339)
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
