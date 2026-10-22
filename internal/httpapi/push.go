package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/push"
)

const (
	pushPeriod       = time.Second
	pushRecheck      = 10 * time.Minute
	pushSendTimeout  = 10 * time.Second
	pushMaxBackoff   = 5 * time.Minute
	pushBatchLimit   = 100
	pushTokenMaxSize = 4096
)

type pushTokenRequest struct {
	Token string `json:"token"`
}

// pushReady reports whether this node can wake phones through FCM. Phones only
// register where it is true, so a node without working credentials costs them
// nothing beyond the normal wake channel.
func (s *Server) pushReady() bool {
	return s.push != nil && s.push.Ready()
}

// putPushToken registers the calling phone's FCM token after FCM itself has
// accepted a test request for it, so a token is stored only when this node can
// actually reach it.
func (s *Server) putPushToken(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	if !s.pushReady() {
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "This server does not send push wake-ups.", false)
		return
	}
	var request pushTokenRequest
	if !decodeOrError(w, r, &request, pushTokenMaxSize+256) {
		return
	}
	token := request.Token
	if token == "" || len(token) > pushTokenMaxSize || strings.ContainsAny(token, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "INVALID_PUSH_TOKEN", "The push token is invalid.", false)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushSendTimeout)
	err := s.push.Send(ctx, token, pushData(principal.DeviceID, "sync"), true)
	cancel()
	switch {
	case errors.Is(err, push.ErrTokenGone):
		writeError(w, http.StatusUnprocessableEntity, "PUSH_TOKEN_REJECTED", "Push cannot reach this token from this server.", false)
		return
	case errors.Is(err, push.ErrMisconfigured):
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "This server does not send push wake-ups.", false)
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNAVAILABLE", "Push is temporarily unavailable.", true)
		return
	}
	// Start from the current events: registering must not announce history.
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO push_tokens(device_id,token,pushed_event_cursor,pushed_call_cursor)
		VALUES ($1,$2,
			COALESCE((SELECT MAX(cursor) FROM server_events WHERE owner_id=$3),0),
			COALESCE((SELECT MAX(cursor) FROM call_events WHERE client_device_id=$1),0))
		ON CONFLICT (device_id) DO UPDATE SET token=EXCLUDED.token,
			pushed_event_cursor=EXCLUDED.pushed_event_cursor, pushed_call_cursor=EXCLUDED.pushed_call_cursor,
			failures=0, next_attempt_at=now(), updated_at=now()`,
		principal.DeviceID, token, principal.OwnerID); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"push": "ready"})
}

func (s *Server) deletePushToken(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "Only client devices receive push wake-ups.", false)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM push_tokens WHERE device_id=$1`, principal.DeviceID); err != nil {
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
	var lastCheck time.Time
	wasReady := s.push.Ready()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !s.push.Ready() && time.Since(lastCheck) >= pushRecheck {
			lastCheck = time.Now()
			if checker, ok := s.push.(interface{ Check(context.Context) error }); ok {
				checkCtx, cancel := context.WithTimeout(ctx, pushSendTimeout)
				_ = checker.Check(checkCtx)
				cancel()
			}
		}
		if ready := s.push.Ready(); ready != wasReady {
			wasReady = ready
			if ready {
				s.logger.Info("push wake-ups available")
			} else {
				s.logger.Warn("push wake-ups unavailable: check FCM_SERVICE_ACCOUNT_FILE")
			}
		}
		if !wasReady {
			continue
		}
		if err := s.pushPending(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("push dispatch failed")
		}
	}
}

type pendingPush struct {
	deviceID                string
	token                   string
	eventCursor, callCursor int64
	pushedCallCursor        int64
	failures                int
}

func (s *Server) pushPending(ctx context.Context) error {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	rows, err := s.db.QueryContext(queryCtx, `
		SELECT device_id::text, token, event_cursor, call_cursor, pushed_call_cursor, failures FROM (
			SELECT t.device_id, t.token, t.failures, t.pushed_event_cursor, t.pushed_call_cursor,
				COALESCE((SELECT MAX(e.cursor) FROM server_events e WHERE e.owner_id=d.owner_id),0) AS event_cursor,
				COALESCE((SELECT MAX(c.cursor) FROM call_events c WHERE c.client_device_id=t.device_id),0) AS call_cursor
			FROM push_tokens t JOIN devices d ON d.id=t.device_id
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
		if err := rows.Scan(&item.deviceID, &item.token, &item.eventCursor, &item.callCursor, &item.pushedCallCursor, &item.failures); err != nil {
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
		sendCtx, stop := context.WithTimeout(ctx, pushSendTimeout)
		kind := "sms"
		if item.callCursor > item.pushedCallCursor {
			kind = "call"
		}
		sendErr := s.push.Send(sendCtx, item.token, pushData(item.deviceID, kind), false)
		stop()
		if ctx.Err() != nil {
			return nil
		}
		var execErr error
		switch {
		case sendErr == nil:
			_, execErr = s.db.ExecContext(ctx, `
				UPDATE push_tokens SET pushed_event_cursor=GREATEST(pushed_event_cursor,$2),
					pushed_call_cursor=GREATEST(pushed_call_cursor,$3), failures=0
				WHERE device_id=$1 AND token=$4`, item.deviceID, item.eventCursor, item.callCursor, item.token)
		case errors.Is(sendErr, push.ErrTokenGone):
			_, execErr = s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE device_id=$1 AND token=$2`, item.deviceID, item.token)
		default:
			delay := pushBackoff(item.failures + 1)
			_, execErr = s.db.ExecContext(ctx, `
				UPDATE push_tokens SET failures=failures+1, next_attempt_at=now()+$2*interval '1 millisecond'
				WHERE device_id=$1 AND token=$3`, item.deviceID, delay.Milliseconds(), item.token)
			if errors.Is(sendErr, push.ErrMisconfigured) {
				return nil
			}
		}
		if execErr != nil {
			return execErr
		}
	}
	return nil
}

// pushData is everything a wake-up carries: what changed and which of the
// phone's pairings it concerns (its device ID on this node), so a phone with
// several nodes asks this one first. No numbers, text or call IDs.
func pushData(deviceID, kind string) map[string]string {
	return map[string]string{"type": "sync", "v": "1", "kind": kind, "device_id": deviceID}
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
