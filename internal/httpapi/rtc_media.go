package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/rtcsignal"
)

// RTCMediaOptions configures the ICE servers handed to both ends of a call.
type RTCMediaOptions struct {
	STUNURLs   []string
	TURNURLs   []string
	TURNSecret string
}

type rtcMediaRuntime struct {
	repo    *rtcsignal.PostgresRepository
	service *rtcsignal.Service
	options RTCMediaOptions
}
type rtcAccessKey struct{}

func (s *Server) startRTCMedia(options RTCMediaOptions) {
	repo, err := rtcsignal.NewPostgresRepository(s.db, rtcMediaAuthorizer{}, s.now)
	if err != nil {
		s.logger.Error("RTC media adapter unavailable")
		return
	}
	service, err := rtcsignal.NewService(repo, s.now)
	if err != nil {
		return
	}
	s.rtc = &rtcMediaRuntime{repo, service, options}
}

// RunBackground ends calls left by SIP releases, then expires RTC signaling
// and call leases until ctx ends. Only the API process runs it.
func (s *Server) RunBackground(ctx context.Context) {
	if s.rtc == nil {
		return
	}
	if count, err := s.EndLegacySIPCalls(ctx); err != nil {
		s.logger.Warn("ending calls left by SIP releases failed")
	} else if count > 0 {
		s.logger.Info("ended calls left by SIP releases", "count", count)
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sweepCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			_, err := s.rtc.repo.Sweep(sweepCtx, 100)
			if err == nil {
				err = s.sweepRTCControl(sweepCtx)
			}
			stop()
			if err != nil && ctx.Err() == nil {
				s.logger.Warn("RTC signal cleanup failed")
			}
		}
	}
}

func rtcContext(r *http.Request) context.Context {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return context.WithValue(r.Context(), rtcAccessKey{}, tokenHash(token))
}
func rtcPrincipal(p Principal) rtcsignal.Principal {
	return rtcsignal.Principal{OwnerID: p.OwnerID, DeviceID: p.DeviceID}
}
func (s *Server) requireRTC(w http.ResponseWriter) bool {
	if s.rtc == nil {
		writeError(w, 503, "RTC_MEDIA_DISABLED", "WebRTC audio is not enabled on this server.", false)
		return false
	}
	return true
}

// Authorize rechecks the exact HTTP session and the same business-call lease on
// every read, SDP command and renewal. An incoming loser cannot access the winner's
// media.
type rtcMediaAuthorizer struct{}

func (a rtcMediaAuthorizer) Authorize(ctx context.Context, tx *sql.Tx, p rtcsignal.Principal, b rtcsignal.Binding) error {
	if p.OwnerID != b.OwnerID || (p.DeviceID != b.ClientID && p.DeviceID != b.GatewayID) {
		return rtcsignal.ErrForbidden
	}
	hash, ok := ctx.Value(rtcAccessKey{}).([]byte)
	if !ok {
		return rtcsignal.ErrForbidden
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,owner_id::text,role,state FROM devices WHERE id IN ($1,$2) ORDER BY id FOR SHARE`, b.ClientID, b.GatewayID)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var id, owner, role, state string
		if err = rows.Scan(&id, &owner, &role, &state); err != nil {
			break
		}
		if owner != b.OwnerID || state != "active" || (id == b.ClientID && role != "client") || (id == b.GatewayID && role != "gateway") {
			err = rtcsignal.ErrForbidden
			break
		}
		count++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if count != 2 {
		return rtcsignal.ErrForbidden
	}
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE device_id=$1 AND access_hash=$2 AND access_expires_at>clock_timestamp())`, p.DeviceID, hash).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return rtcsignal.ErrForbidden
	}
	var gateway, sim, client, direction, state, winner, transport string
	var revision int64
	var expiry sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT gateway_id::text,sim_id::text,client_device_id::text,direction,state,COALESCE(incoming_winner_client_device_id::text,''),mapping_revision,expires_at,control_transport FROM call_sessions WHERE call_id=$1 AND owner_id=$2 FOR SHARE`, b.CallID, b.OwnerID).Scan(&gateway, &sim, &client, &direction, &state, &winner, &revision, &expiry, &transport)
	if errors.Is(err, sql.ErrNoRows) {
		return rtcsignal.ErrForbidden
	}
	if err != nil {
		return err
	}
	if transport != "rtc" || gateway != b.GatewayID || sim != b.SIMID || revision != b.MappingRevision || state == "ended" || state == "unknown" {
		return rtcsignal.ErrForbidden
	}
	if state == "reserved" && expiry.Valid && !expiry.Time.After(time.Now()) {
		return rtcsignal.ErrExpired
	}
	var candidate string
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN candidate_until>clock_timestamp() THEN COALESCE(candidate_client_id::text,'') ELSE '' END,NOT end_requested AND gateway_seen_at>clock_timestamp()-interval '20 seconds' AND (c.direction='incoming' AND c.incoming_winner_client_device_id IS NULL OR client_seen_at>clock_timestamp()-interval '20 seconds') FROM rtc_control_calls t JOIN call_sessions c ON c.call_id=t.call_id WHERE t.call_id=$1`, b.CallID).Scan(&candidate, &allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return rtcsignal.ErrForbidden
	}
	if direction == "incoming" && winner == "" {
		if candidate != b.ClientID {
			return rtcsignal.ErrForbidden
		}
		// Only permits this bounded reservation to negotiate. Winner/PCM
		// gating remains a separate control operation owned by the gateway.
		winner = candidate
	}
	if direction == "outgoing" {
		if client != b.ClientID {
			return rtcsignal.ErrForbidden
		}
	} else {
		if winner != "" && winner != b.ClientID {
			return rtcsignal.ErrForbidden
		}
		if p.DeviceID == b.GatewayID && winner != b.ClientID {
			return rtcsignal.ErrConflict
		}
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_participants WHERE call_id=$1 AND client_device_id=$2 AND owner_id=$3 AND state<>'ended')`, b.CallID, b.ClientID, b.OwnerID).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return rtcsignal.ErrForbidden
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_call_slots cs JOIN gateways g ON g.device_id=cs.gateway_id JOIN sim_bindings sb ON sb.gateway_id=g.device_id WHERE cs.gateway_id=$1 AND cs.call_id=$2 AND sb.sim_id=$3 AND sb.mapping_revision=$4 AND g.mapping_revision=$4 AND sb.state='active' AND sb.identity_verified)`, b.GatewayID, b.CallID, b.SIMID, b.MappingRevision).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return rtcsignal.ErrForbidden
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rtc_media_sessions WHERE call_id=$1 AND client_device_id=$2 AND leg_id=$3 AND attempt_id=$4)`, b.CallID, b.ClientID, b.LegID, b.AttemptID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return rtcsignal.ErrForbidden
	}
	return nil
}

func (s *Server) rtcCapability(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTC(w) {
		return
	}
	if p.Role != "gateway" {
		writeError(w, 403, "GATEWAY_REQUIRED", "Gateway required.", false)
		return
	}
	var body struct {
		Version int `json:"version"`
	}
	if !decodeOrError(w, r, &body, 1024) {
		return
	}
	if body.Version != 1 {
		writeError(w, 400, "RTC_VERSION_UNSUPPORTED", "Unsupported RTC audio version.", false)
		return
	}
	_, err := s.db.ExecContext(r.Context(), `INSERT INTO rtc_media_capabilities(device_id,version) VALUES($1,1) ON CONFLICT(device_id) DO UPDATE SET version=1,last_seen_at=clock_timestamp()`, p.DeviceID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "media_transport": "webrtc"})
}

// selectRTCMedia creates the client's media leg before the call is dialed or
// answered. Retrying returns the same leg. No cellular action occurs here.
func (s *Server) selectRTCMedia(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTC(w) {
		return
	}
	if p.Role != "client" {
		writeError(w, 403, "CLIENT_REQUIRED", "Client required.", false)
		return
	}
	var body struct {
		MediaTransport string `json:"media_transport"`
	}
	if !decodeOrError(w, r, &body, 1024) {
		return
	}
	if body.MediaTransport != "webrtc" {
		writeError(w, 400, "RTC_MODE_INVALID", "Only WebRTC admission is accepted here.", false)
		return
	}
	callID := strings.ToLower(r.PathValue("call_id"))
	if !validUUID(callID) {
		rtcError(w, rtcsignal.ErrInvalid)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `SET LOCAL synchronous_commit=on`); err != nil {
		writeDBUnavailable(w)
		return
	}
	var b rtcsignal.Binding
	var direction, state, client, winner, transport string
	var valid bool
	b.CallID = callID
	b.OwnerID = p.OwnerID
	b.ClientID = p.DeviceID
	err = tx.QueryRowContext(r.Context(), `SELECT gateway_id::text,sim_id::text,mapping_revision,direction,state,client_device_id::text,COALESCE(incoming_winner_client_device_id::text,''),control_transport FROM call_sessions WHERE call_id=$1 AND owner_id=$2 FOR UPDATE`, callID, p.OwnerID).Scan(&b.GatewayID, &b.SIMID, &b.MappingRevision, &direction, &state, &client, &winner, &transport)
	if err != nil {
		rtcError(w, rtcsignal.ErrForbidden)
		return
	}
	if transport != "rtc" || state == "ended" || state == "unknown" || (direction == "outgoing" && client != p.DeviceID) || (winner != "" && winner != p.DeviceID) {
		rtcError(w, rtcsignal.ErrForbidden)
		return
	}
	if direction == "incoming" {
		err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM call_participants WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended')`, callID, p.DeviceID).Scan(&valid)
		if err != nil || !valid {
			rtcError(w, rtcsignal.ErrForbidden)
			return
		}
	}
	err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM rtc_media_capabilities WHERE device_id=$1 AND version=1 AND last_seen_at>clock_timestamp()-interval '90 seconds')`, b.GatewayID).Scan(&valid)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if !valid {
		writeError(w, 409, "RTC_GATEWAY_UNAVAILABLE", "Gateway does not currently advertise WebRTC audio.", false)
		return
	}
	var alreadySelected bool
	err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM rtc_media_sessions WHERE call_id=$1 AND client_device_id=$2)`, callID, p.DeviceID).Scan(&alreadySelected)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if !alreadySelected && ((direction == "outgoing" && state != "reserved" && state != "connecting") || (direction == "incoming" && (winner != "" || state == "active"))) {
		writeError(w, 409, "RTC_MODE_FROZEN", "Audio must be set up before the call is dialed or answered.", false)
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO rtc_media_sessions(call_id,client_device_id,leg_id,attempt_id) VALUES($1,$2,$3,$4) ON CONFLICT(call_id,client_device_id) DO NOTHING`, callID, p.DeviceID, newUUID(), newUUID())
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	err = tx.QueryRowContext(r.Context(), `SELECT leg_id,attempt_id FROM rtc_media_sessions WHERE call_id=$1 AND client_device_id=$2`, callID, p.DeviceID).Scan(&b.LegID, &b.AttemptID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if err = tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	ctx := rtcContext(r)
	err = s.rtc.repo.Create(ctx, rtcPrincipal(p), b, s.now().Add(60*time.Second))
	if err != nil && !errors.Is(err, rtcsignal.ErrConflict) {
		rtcError(w, err)
		return
	}
	result, err := s.rtc.service.Read(ctx, rtcPrincipal(p), b.LegID)
	if err != nil {
		rtcError(w, err)
		return
	}
	if result.Snapshot.Phase == rtcsignal.Closed {
		rtcError(w, rtcsignal.ErrClosed)
		return
	}
	s.logger.Info("call media selected", "call_id", callID, "media", "webrtc", "rtc_signaling", "https", "leg_id", b.LegID)
	s.writeMediaAdmission(w, b, false)
}

func (s *Server) getRTCMedia(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTC(w) {
		return
	}
	callID := strings.ToLower(r.PathValue("call_id"))
	if !validUUID(callID) {
		rtcError(w, rtcsignal.ErrInvalid)
		return
	}
	var b rtcsignal.Binding
	var direction, state, client, winner, transport string
	b.OwnerID = p.OwnerID
	b.CallID = callID
	err := s.db.QueryRowContext(r.Context(), `SELECT gateway_id::text,sim_id::text,mapping_revision,direction,state,client_device_id::text,COALESCE(incoming_winner_client_device_id::text,''),control_transport FROM call_sessions WHERE call_id=$1 AND owner_id=$2`, callID, p.OwnerID).Scan(&b.GatewayID, &b.SIMID, &b.MappingRevision, &direction, &state, &client, &winner, &transport)
	if err != nil || transport != "rtc" || state == "ended" || state == "unknown" {
		rtcError(w, rtcsignal.ErrForbidden)
		return
	}
	if p.Role == "gateway" {
		if b.GatewayID != p.DeviceID {
			rtcError(w, rtcsignal.ErrForbidden)
			return
		}
		if direction == "incoming" {
			if winner == "" {
				_ = s.db.QueryRowContext(r.Context(), `SELECT COALESCE(candidate_client_id::text,'') FROM rtc_control_calls WHERE call_id=$1 AND candidate_until>clock_timestamp()`, callID).Scan(&winner)
			}
			if winner == "" {
				writeJSON(w, 200, map[string]string{"media_transport": "pending"})
				return
			}
			client = winner
		}
	} else if p.Role == "client" {
		if direction == "outgoing" && client != p.DeviceID {
			rtcError(w, rtcsignal.ErrForbidden)
			return
		}
		if direction == "incoming" {
			var participant bool
			err = s.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM call_participants WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3 AND state<>'ended')`, callID, p.OwnerID, p.DeviceID).Scan(&participant)
			if err != nil || !participant || (winner != "" && winner != p.DeviceID) {
				rtcError(w, rtcsignal.ErrForbidden)
				return
			}
		}
		client = p.DeviceID
	} else {
		rtcError(w, rtcsignal.ErrForbidden)
		return
	}
	b.ClientID = client
	var active bool
	err = s.db.QueryRowContext(r.Context(), `SELECT leg_id,attempt_id,cellular_active FROM rtc_media_sessions WHERE call_id=$1 AND client_device_id=$2`, callID, client).Scan(&b.LegID, &b.AttemptID, &active)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 200, map[string]string{"media_transport": "pending"})
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	result, err := s.rtc.service.Read(rtcContext(r), rtcPrincipal(p), b.LegID)
	if err != nil {
		rtcError(w, err)
		return
	}
	if result.Snapshot.Phase == rtcsignal.Closed {
		rtcError(w, rtcsignal.ErrClosed)
		return
	}
	s.writeMediaAdmission(w, b, active)
}

func (s *Server) writeMediaAdmission(w http.ResponseWriter, b rtcsignal.Binding, active bool) {
	servers := []map[string]any{}
	if len(s.rtc.options.STUNURLs) > 0 {
		servers = append(servers, map[string]any{"urls": s.rtc.options.STUNURLs})
	}
	if len(s.rtc.options.TURNURLs) > 0 && s.rtc.options.TURNSecret != "" {
		username := fmt.Sprintf("%d:%s:%s", s.now().Add(time.Hour).Unix(), b.ClientID, b.CallID)
		mac := hmac.New(sha1.New, []byte(s.rtc.options.TURNSecret))
		_, _ = mac.Write([]byte(username))
		servers = append(servers, map[string]any{"urls": s.rtc.options.TURNURLs, "username": username, "credential": base64.StdEncoding.EncodeToString(mac.Sum(nil))})
	}
	writeJSON(w, 200, map[string]any{"media_transport": "webrtc", "call_control": "rtc", "rtc_signaling": "https", "binding": b, "cellular_active": active, "ice_servers": servers})
}

func (s *Server) rtcSignals(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTC(w) {
		return
	}
	legID := r.PathValue("leg_id")
	ctx := rtcContext(r)
	actor := rtcPrincipal(p)
	if r.Method == http.MethodPost {
		data, err := io.ReadAll(io.LimitReader(r.Body, rtcsignal.MaxFrameBytes+1))
		if err != nil {
			rtcError(w, rtcsignal.ErrInvalid)
			return
		}
		command, err := rtcsignal.DecodeCommand(data)
		if err != nil || command.LegID != legID {
			rtcError(w, rtcsignal.ErrInvalid)
			return
		}
		ack, duplicate, err := s.rtc.service.Apply(ctx, actor, command)
		if err != nil {
			rtcError(w, err)
			return
		}
		if command.Kind != rtcsignal.Candidate && command.Kind != rtcsignal.EndCandidates {
			s.logger.Info("RTC signal committed", "call_id", command.CallID, "leg_id", legID, "media", "webrtc", "event", command.Kind, "duplicate", duplicate)
		}
		writeJSON(w, 200, map[string]any{"ack": ack, "duplicate": duplicate})
		return
	}
	// Renew only after authorizer rechecks the exact current session and shared
	// call lease. Never renew a closed/expired grant or extend a negotiation.
	err := s.rtc.repo.Update(ctx, actor, legID, func(l rtcsignal.Leg) (rtcsignal.Leg, error) {
		snap := l.Snapshot()
		now := s.now()
		if snap.Phase != rtcsignal.Closed && snap.AuthorizedUntil.Sub(now) < 30*time.Second {
			return l.ExtendAuthorization(now, now.Add(60*time.Second))
		}
		return l, nil
	})
	if err != nil {
		rtcError(w, err)
		return
	}
	result, err := s.rtc.service.Read(ctx, actor, legID)
	if err != nil {
		rtcError(w, err)
		return
	}
	var active bool
	if err = s.db.QueryRowContext(r.Context(), `SELECT cellular_active FROM rtc_media_sessions WHERE leg_id=$1`, legID).Scan(&active); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, 200, map[string]any{"state": result, "cellular_active": active})
}

func (s *Server) rtcCellularActive(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTC(w) {
		return
	}
	if p.Role != "gateway" {
		rtcError(w, rtcsignal.ErrForbidden)
		return
	}
	var body struct {
		LegID string `json:"leg_id"`
	}
	if !decodeOrError(w, r, &body, 1024) {
		return
	}
	result, err := s.rtc.service.Read(rtcContext(r), rtcPrincipal(p), body.LegID)
	if err != nil {
		rtcError(w, err)
		return
	}
	b := result.Snapshot.Binding
	if b.CallID != r.PathValue("call_id") || b.GatewayID != p.DeviceID || result.Snapshot.Phase != rtcsignal.Negotiated {
		rtcError(w, rtcsignal.ErrConflict)
		return
	}
	updated, err := s.db.ExecContext(r.Context(), `UPDATE rtc_media_sessions m SET cellular_active=true FROM call_sessions c WHERE m.leg_id=$1 AND m.call_id=c.call_id AND c.state IN ('dialing','connecting','active') AND c.control_transport='rtc' AND ((c.direction='incoming' AND c.incoming_winner_client_device_id=m.client_device_id) OR (c.direction='outgoing' AND EXISTS(SELECT 1 FROM rtc_control_calls t WHERE t.call_id=c.call_id AND t.dispatched AND NOT t.end_requested)))`, b.LegID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		rtcError(w, rtcsignal.ErrConflict)
		return
	}
	s.logger.Info("RTC cellular media active", "call_id", b.CallID, "leg_id", b.LegID, "media", "webrtc")
	writeJSON(w, 200, map[string]bool{"cellular_active": true})
}

func rtcError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rtcsignal.ErrInvalid):
		writeError(w, 400, "RTC_INVALID", "Invalid RTC operation.", false)
	case errors.Is(err, rtcsignal.ErrForbidden):
		writeError(w, 403, "RTC_FORBIDDEN", "RTC call authority is unavailable.", false)
	case errors.Is(err, rtcsignal.ErrConflict):
		writeError(w, 409, "RTC_CONFLICT", "RTC state changed; read the authoritative snapshot.", true)
	case errors.Is(err, rtcsignal.ErrClosed), errors.Is(err, rtcsignal.ErrExpired):
		writeError(w, 410, "RTC_ENDED", "RTC authorization ended.", false)
	case errors.Is(err, rtcsignal.ErrLimit):
		writeError(w, 429, "RTC_LIMIT", "RTC signal budget exceeded.", false)
	default:
		writeDBUnavailable(w)
	}
}
