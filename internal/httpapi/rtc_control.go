package httpapi

// Call control. Cellular side effects remain exclusively owned by the
// gateway's durable, installation-wide dispatch/winner ledger.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/calls"
	"github.com/kirikira/gsm2sip-server/internal/rtcsignal"
)

type rtcControlSnapshot struct {
	calls.Call
	Version          int        `json:"version"`
	ControlTransport string     `json:"call_control"`
	EndRequested     bool       `json:"end_requested"`
	Dispatched       bool       `json:"dispatched"`
	CandidateID      string     `json:"candidate_client_id,omitempty"`
	WinnerID         string     `json:"winner_client_id,omitempty"`
	CandidateUntil   *time.Time `json:"candidate_until,omitempty"`
}
type rtcCreateCall struct {
	Version         int    `json:"version"`
	OperationID     string `json:"operation_id"`
	CallID          string `json:"call_id"`
	GatewayID       string `json:"gateway_id"`
	SIMID           string `json:"sim_id"`
	MappingRevision int64  `json:"mapping_revision"`
	To              string `json:"to"`
	From            string `json:"from,omitempty"`
}
type rtcCallOperation struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	Revision    int64  `json:"revision"`
	Kind        string `json:"kind"`
	CandidateID string `json:"candidate_client_id,omitempty"`
}

func (s *Server) requireRTCControl(w http.ResponseWriter) bool {
	if s.rtc == nil {
		writeError(w, 503, "RTC_CONTROL_DISABLED", "Call control is not available.", false)
		return false
	}
	return true
}
func (s *Server) rtcControlCapability(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTCControl(w) {
		return
	}
	var body struct {
		Version int   `json:"version"`
		Enabled *bool `json:"enabled"`
	}
	if !decodeOrError(w, r, &body, 1024) {
		return
	}
	if body.Version != 1 || body.Enabled == nil {
		writeCallError(w, calls.ErrInvalid)
		return
	}
	_, err := s.db.ExecContext(r.Context(), `INSERT INTO rtc_control_capabilities(device_id,version,enabled) VALUES($1,1,$2) ON CONFLICT(device_id) DO UPDATE SET version=1,enabled=$2,last_seen_at=clock_timestamp()`, p.DeviceID, *body.Enabled)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "call_control": "rtc", "enabled": body.Enabled})
}

// Serialize against credential revocation before locking call rows, matching the
// media authorizer's device -> call ordering. Recheck the exact session token.
func (s *Server) rtcControlIdentity(ctx context.Context, tx *sql.Tx, r *http.Request, p Principal, gateway string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM devices WHERE owner_id=$1 AND state='active' AND (id=$2 OR id=$3) ORDER BY id FOR SHARE`, p.OwnerID, p.DeviceID, gateway)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
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
	want := 2
	if gateway == p.DeviceID {
		want = 1
	}
	if count != want {
		return calls.ErrForbidden
	}
	hash := rtcContext(r).Value(rtcAccessKey{}).([]byte)
	var ok bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE device_id=$1 AND access_hash=$2 AND access_expires_at>clock_timestamp())`, p.DeviceID, hash).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return calls.ErrForbidden
	}
	return nil
}

func controlSnapshot(ctx context.Context, tx *sql.Tx, p Principal, id string, lock bool) (rtcControlSnapshot, error) {
	var out rtcControlSnapshot
	query := `SELECT c.call_id::text,c.gateway_id::text,c.client_device_id::text,c.sim_id::text,c.mapping_revision,c.direction,c.state,c.state_revision,COALESCE(c.from_address,''),c.to_address,COALESCE(c.reason,''),c.created_at,c.expires_at,c.answered_at,t.end_requested,t.dispatched,COALESCE(t.candidate_client_id::text,''),COALESCE(c.incoming_winner_client_device_id::text,''),t.candidate_until
      FROM call_sessions c JOIN rtc_control_calls t ON t.call_id=c.call_id
      WHERE c.call_id=$1 AND c.owner_id=$2 AND c.control_transport='rtc'
        AND (c.gateway_id=$3 OR (c.direction='outgoing' AND c.client_device_id=$3) OR EXISTS(SELECT 1 FROM call_participants p WHERE p.call_id=c.call_id AND p.client_device_id=$3))`
	if lock {
		query += ` FOR UPDATE OF c,t`
	}
	err := tx.QueryRowContext(ctx, query, id, p.OwnerID, p.DeviceID).Scan(&out.CallID, &out.GatewayID, &out.ClientID, &out.SIMID, &out.MappingRevision, &out.Direction, &out.State, &out.StateRevision, &out.From, &out.To, &out.Reason, &out.CreatedAt, &out.ExpiresAt, &out.AnsweredAt, &out.EndRequested, &out.Dispatched, &out.CandidateID, &out.WinnerID, &out.CandidateUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return out, calls.ErrNotFound
	}
	out.Version = 1
	out.ControlTransport = "rtc"
	if err == nil && p.Role == "client" && out.Direction == "incoming" {
		var state, reason string
		err = tx.QueryRowContext(ctx, `SELECT state,COALESCE(reason,'') FROM call_participants WHERE call_id=$1 AND client_device_id=$2`, id, p.DeviceID).Scan(&state, &reason)
		if state == "ended" {
			out.State = "ended"
			out.Reason = optionalString(reason)
		}
		out.ClientID = p.DeviceID
	}
	return out, err
}

// A repeated command returns its committed ACK, before checking revision or
// terminal state. A reused operation id with different content is a conflict.
func controlReplay(ctx context.Context, tx *sql.Tx, p Principal, op, id string, payload any) (json.RawMessage, bool, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	hash := sha256.Sum256(data)
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.DeviceID+":"+op)
	if err != nil {
		return nil, false, err
	}
	var oldHash, result []byte
	var oldID string
	err = tx.QueryRowContext(ctx, `SELECT call_id::text,payload_hash,result FROM rtc_control_operations WHERE device_id=$1 AND operation_id=$2`, p.DeviceID, op).Scan(&oldID, &oldHash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if oldID != id || !bytes.Equal(oldHash, hash[:]) {
		return nil, false, calls.ErrConflict
	}
	return result, true, nil
}
func controlACK(ctx context.Context, tx *sql.Tx, p Principal, op string, payload any, out rtcControlSnapshot) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	result, err := json.Marshal(out)
	if err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM rtc_control_operations WHERE call_id=$1`, out.CallID).Scan(&count); err != nil {
		return err
	}
	if count >= 512 {
		return calls.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rtc_control_operations(device_id,operation_id,call_id,payload_hash,result) VALUES($1,$2,$3,$4,$5)`, p.DeviceID, op, out.CallID, hash[:], string(result))
	return err
}
func controlEvent(ctx context.Context, tx *sql.Tx, id string) error {
	// The existing event/history feed remains usable; no SMS cursor is used to
	// carry call commands, SDP, or audio. Each client has its own event row.
	_, err := tx.ExecContext(ctx, `INSERT INTO call_events(call_id,owner_id,client_device_id,state_revision,event_type,event_json)
      SELECT c.call_id,c.owner_id,c.client_device_id,c.state_revision,'call.'||c.state,jsonb_build_object('state',c.state,'call_control','rtc') FROM call_sessions c WHERE c.call_id=$1 AND c.direction='outgoing'
      UNION ALL
      SELECT c.call_id,c.owner_id,p.client_device_id,c.state_revision,'call.'||c.state,jsonb_build_object('state',CASE WHEN p.state='ended' THEN 'ended' ELSE c.state END,'reason',COALESCE(p.reason,c.reason),'call_control','rtc') FROM call_sessions c JOIN call_participants p ON p.call_id=c.call_id WHERE c.call_id=$1 AND c.direction='incoming'
      ON CONFLICT(call_id,client_device_id,state_revision) DO NOTHING`, id)
	return err
}

func (s *Server) rtcControlCreate(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTCControl(w) {
		return
	}
	incoming := r.URL.Path == "/v1/rtc/control/incoming"
	if (incoming && p.Role != "gateway") || (!incoming && p.Role != "client") {
		writeCallError(w, calls.ErrForbidden)
		return
	}
	var body rtcCreateCall
	if !decodeOrError(w, r, &body, 2048) {
		return
	}
	body.OperationID = strings.ToLower(body.OperationID)
	body.CallID = strings.ToLower(body.CallID)
	body.GatewayID = strings.ToLower(body.GatewayID)
	body.SIMID = strings.ToLower(body.SIMID)
	if body.Version != 1 || !validUUID(body.OperationID) || !validUUID(body.CallID) || !validUUID(body.GatewayID) || !validUUID(body.SIMID) || body.MappingRevision < 1 || (!incoming && (!validAddress(body.To) || body.From != "")) || (incoming && (len(body.To) > 64 || strings.ContainsAny(body.To, "\r\n"))) || len(body.From) > 64 || strings.ContainsAny(body.From, "\r\n") || (incoming && body.GatewayID != p.DeviceID) {
		writeCallError(w, calls.ErrInvalid)
		return
	}
	ctx := r.Context()
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	fail := func(err error) { writeCallError(w, err) }
	if err = s.rtcControlIdentity(ctx, tx, r, p, body.GatewayID); err != nil {
		fail(err)
		return
	}
	replay, found, err := controlReplay(ctx, tx, p, body.OperationID, body.CallID, body)
	if err != nil {
		fail(err)
		return
	}
	if found {
		if err = tx.Commit(); err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, replay)
		return
	}
	// Lock the gateway and physical SIM binding before taking the shared slot.
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT g.mapping_revision=$4 AND sb.mapping_revision=$4 AND sb.state='active' AND sb.identity_verified AND g.last_seen_at>clock_timestamp()-interval '90 seconds'
      FROM gateways g JOIN devices d ON d.id=g.device_id JOIN sim_bindings sb ON sb.gateway_id=g.device_id
      WHERE g.device_id=$1 AND d.owner_id=$2 AND d.state='active' AND sb.sim_id=$3 AND sb.owner_id=$2 FOR UPDATE OF g,sb`, body.GatewayID, p.OwnerID, body.SIMID, body.MappingRevision).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !valid {
		fail(calls.ErrSIMUnavailable)
		return
	}
	if err != nil {
		fail(err)
		return
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rtc_control_capabilities WHERE device_id=$1 AND enabled AND last_seen_at>clock_timestamp()-interval '90 seconds')`, body.GatewayID).Scan(&valid); err != nil {
		fail(err)
		return
	}
	if !valid {
		fail(calls.ErrGatewayUnavailable)
		return
	}
	var occupied bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_call_slots WHERE gateway_id=$1)`, body.GatewayID).Scan(&occupied); err != nil {
		fail(err)
		return
	}
	if occupied {
		fail(calls.ErrBusy)
		return
	}
	client := p.DeviceID
	clients := []string{client}
	state := "reserved"
	direction := "outgoing"
	seconds := 30
	if incoming {
		clients = nil
		state = "ringing"
		direction = "incoming"
		seconds = 25
		// Freeze the whole incoming fork before dispatch: it rings every client
		// that currently advertises call control. Others never see the call.
		rows, e := tx.QueryContext(ctx, `SELECT d.id::text FROM devices d JOIN rtc_control_capabilities c ON c.device_id=d.id WHERE d.owner_id=$1 AND d.role='client' AND d.state='active' AND c.enabled AND c.last_seen_at>clock_timestamp()-interval '90 seconds' AND EXISTS(SELECT 1 FROM sessions ss WHERE ss.device_id=d.id AND ss.refresh_expires_at>clock_timestamp()) ORDER BY d.id`, p.OwnerID)
		if e != nil {
			fail(e)
			return
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			clients = append(clients, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			fail(err)
			return
		}
		if len(clients) == 0 {
			fail(calls.ErrGatewayUnavailable)
			return
		}
		client = clients[0]
	} else {
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rtc_control_capabilities WHERE device_id=$1 AND enabled AND last_seen_at>clock_timestamp()-interval '90 seconds')`, client).Scan(&valid); err != nil {
			fail(err)
			return
		}
		if !valid {
			fail(calls.ErrForbidden)
			return
		}
	}
	var used bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_sessions WHERE call_id=$1)`, body.CallID).Scan(&used); err != nil {
		fail(err)
		return
	}
	if used {
		fail(calls.ErrCallIDConflict)
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,from_address,state,expires_at,gateway_endpoint_id,client_endpoint_id,control_transport) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,clock_timestamp()+($11*interval '1 second'),'','','rtc')`, body.CallID, p.OwnerID, client, body.GatewayID, body.SIMID, body.MappingRevision, direction, body.To, body.From, state, seconds)
	if err != nil {
		fail(err)
		return
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rtc_control_calls(call_id) VALUES($1)`, body.CallID); err != nil {
		fail(err)
		return
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, body.GatewayID, body.CallID); err != nil {
		fail(err)
		return
	}
	if incoming {
		for _, id := range clients {
			if _, err = tx.ExecContext(ctx, `INSERT INTO call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state) VALUES($1,$2,$3,'',$4,'ringing')`, body.CallID, p.OwnerID, id, newUUID()); err != nil {
				fail(err)
				return
			}
		}
	}
	if err = controlEvent(ctx, tx, body.CallID); err != nil {
		fail(err)
		return
	}
	out, err := controlSnapshot(ctx, tx, p, body.CallID, false)
	if err != nil {
		fail(err)
		return
	}
	if err = controlACK(ctx, tx, p, body.OperationID, body, out); err != nil {
		fail(err)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(err)
		return
	}
	s.logger.Info("RTC call created", "call_id", out.CallID, "call_control", "rtc", "direction", direction)
	writeJSON(w, 201, out)
}

func controlBump(ctx context.Context, tx *sql.Tx, id, state, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state=$2,reason=NULLIF($3,''),state_revision=state_revision+1,updated_at=clock_timestamp(),answered_at=CASE WHEN $2='active' THEN COALESCE(answered_at,clock_timestamp()) ELSE answered_at END,ended_at=CASE WHEN $2='ended' THEN clock_timestamp() ELSE NULL END WHERE call_id=$1`, id, state, reason)
	if err != nil {
		return err
	}
	return controlEvent(ctx, tx, id)
}
func controlEnd(ctx context.Context, tx *sql.Tx, id, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE call_participants SET state='ended',reason=COALESCE(reason,$2),ended_at=COALESCE(ended_at,clock_timestamp()),state_revision=state_revision+1,updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, id, reason); err != nil {
		return err
	}
	if err := controlBump(ctx, tx, id, "ended", reason); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE call_id=$1`, id)
	return err
}
func controlRequestEnd(ctx context.Context, tx *sql.Tx, out rtcControlSnapshot, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE rtc_control_calls SET end_requested=true WHERE call_id=$1`, out.CallID); err != nil {
		return err
	}
	// An outgoing reservation never exposed to cellular dispatch can safely
	// release immediately. Otherwise retain occupancy until real gateway END.
	if out.Direction == "outgoing" && !out.Dispatched {
		return controlEnd(ctx, tx, out.CallID, reason)
	}
	return controlBump(ctx, tx, out.CallID, out.State, reason)
}
func controlMediaNegotiated(ctx context.Context, tx *sql.Tx, id, client string) error {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT l.checkpoint FROM rtc_media_sessions m JOIN rtc_signal_legs l ON l.leg_id=m.leg_id WHERE m.call_id=$1 AND m.client_device_id=$2`, id, client).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return calls.ErrConflict
	}
	if err != nil {
		return err
	}
	if !rtcsignal.CheckpointNegotiated(data, time.Now()) {
		return calls.ErrConflict
	}
	return nil
}

func (s *Server) rtcControlOperate(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTCControl(w) {
		return
	}
	id := strings.ToLower(r.PathValue("call_id"))
	var body rtcCallOperation
	if !decodeOrError(w, r, &body, 2048) {
		return
	}
	body.OperationID = strings.ToLower(body.OperationID)
	body.CandidateID = strings.ToLower(body.CandidateID)
	if !validUUID(id) || body.Version != 1 || !validUUID(body.OperationID) || body.Revision < 1 || (body.CandidateID != "" && !validUUID(body.CandidateID)) {
		writeCallError(w, calls.ErrInvalid)
		return
	}
	ctx := r.Context()
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	fail := func(err error) { writeCallError(w, err) }
	// Obtain scope without a call lock, then follow device -> call lock order.
	initial, err := controlSnapshot(ctx, tx, p, id, false)
	if err != nil {
		fail(err)
		return
	}
	if err = s.rtcControlIdentity(ctx, tx, r, p, initial.GatewayID); err != nil {
		fail(err)
		return
	}
	replay, found, err := controlReplay(ctx, tx, p, body.OperationID, id, body)
	if err != nil {
		fail(err)
		return
	}
	if found {
		if err = tx.Commit(); err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, replay)
		return
	}
	out, err := controlSnapshot(ctx, tx, p, id, true)
	if err != nil {
		fail(err)
		return
	}
	if out.State == "ended" || (out.State == "unknown" && !(p.Role == "gateway" && body.Kind == "ended")) || out.StateRevision != body.Revision {
		fail(calls.ErrConflict)
		return
	}
	if out.EndRequested && !(body.Kind == "ended" || body.Kind == "hangup" || body.Kind == "cancel") {
		fail(calls.ErrConflict)
		return
	}
	if out.State != "active" && out.ExpiresAt != nil && !out.ExpiresAt.After(s.now()) && body.Kind != "ended" && body.Kind != "hangup" && body.Kind != "cancel" && body.Kind != "reject" {
		fail(calls.ErrConflict)
		return
	}
	isGateway := p.Role == "gateway" && p.DeviceID == out.GatewayID
	isClient := p.Role == "client"
	switch body.Kind {
	case "start":
		if !isClient || out.Direction != "outgoing" || out.State != "reserved" {
			err = calls.ErrConflict
			break
		}
		err = controlBump(ctx, tx, id, "connecting", "")
	case "accept":
		if !isClient || out.Direction != "incoming" || out.WinnerID != "" || out.EndRequested || out.ExpiresAt == nil || !out.ExpiresAt.After(s.now()) {
			err = calls.ErrConflict
			break
		}
		if out.CandidateID != "" && out.CandidateID != p.DeviceID && out.CandidateUntil != nil && out.CandidateUntil.After(s.now()) {
			err = calls.ErrBusy
			break
		}
		_, err = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET candidate_client_id=$2,candidate_until=LEAST(clock_timestamp()+interval '20 seconds',(SELECT expires_at FROM call_sessions WHERE call_id=$1)),client_seen_at=clock_timestamp() WHERE call_id=$1`, id, p.DeviceID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE call_participants SET state='connecting',state_revision=state_revision+1,updated_at=clock_timestamp() WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended'`, id, p.DeviceID)
		}
		if err == nil {
			err = controlBump(ctx, tx, id, "connecting", "")
		}
	case "release":
		if !isClient || out.Direction != "incoming" || out.CandidateID != p.DeviceID || out.WinnerID != "" {
			err = calls.ErrConflict
			break
		}
		_, err = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET candidate_client_id=NULL,candidate_until=NULL WHERE call_id=$1`, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE call_participants SET state='ringing',state_revision=state_revision+1 WHERE call_id=$1 AND client_device_id=$2`, id, p.DeviceID)
		}
		if err == nil {
			err = controlBump(ctx, tx, id, "ringing", "")
		}
	case "winner":
		// The gateway submits this only after its installation-wide durable
		// winner CAS. A single node reservation is not a cellular authorization.
		if !isGateway || out.Direction != "incoming" || out.WinnerID != "" || body.CandidateID != out.CandidateID || out.CandidateID == "" || out.CandidateUntil == nil || !out.CandidateUntil.After(s.now()) {
			err = calls.ErrConflict
			break
		}
		if err = controlMediaNegotiated(ctx, tx, id, out.CandidateID); err != nil {
			break
		}
		_, err = tx.ExecContext(ctx, `UPDATE call_sessions SET incoming_winner_client_device_id=$2,client_device_id=$2 WHERE call_id=$1`, id, out.CandidateID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE call_participants SET state=CASE WHEN client_device_id=$2 THEN 'accepted' ELSE 'ended' END,reason=CASE WHEN client_device_id=$2 THEN NULL ELSE 'answered_elsewhere' END,accepted_at=CASE WHEN client_device_id=$2 THEN clock_timestamp() END,ended_at=CASE WHEN client_device_id<>$2 THEN clock_timestamp() END,state_revision=state_revision+1 WHERE call_id=$1 AND state<>'ended'`, id, out.CandidateID)
		}
		if err == nil {
			err = controlBump(ctx, tx, id, "connecting", "")
		}
	case "dispatch":
		if !isGateway || out.Direction != "outgoing" || out.State != "connecting" || out.Dispatched {
			err = calls.ErrConflict
			break
		}
		if out.ExpiresAt == nil || !out.ExpiresAt.After(s.now()) {
			err = calls.ErrConflict
			break
		}
		if err = controlMediaNegotiated(ctx, tx, id, out.ClientID); err != nil {
			break
		}
		_, err = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET dispatched=true WHERE call_id=$1`, id)
		if err == nil {
			err = controlBump(ctx, tx, id, "dialing", "")
		}
	case "ringing":
		if !isGateway || !out.Dispatched || out.State != "dialing" {
			err = calls.ErrConflict
			break
		}
		err = controlBump(ctx, tx, id, "ringing", "")
	case "active":
		if !isGateway || (out.Direction == "outgoing" && !out.Dispatched) || (out.Direction == "incoming" && out.WinnerID == "") {
			err = calls.ErrConflict
			break
		}
		client := out.ClientID
		if out.Direction == "incoming" {
			client = out.WinnerID
		}
		if err = controlMediaNegotiated(ctx, tx, id, client); err != nil {
			break
		}
		// Only the gateway's real Telecom ACTIVE + started PCM report sets
		// cellular_active. ICE connected alone never activates a business call.
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT cellular_active FROM rtc_media_sessions WHERE call_id=$1 AND client_device_id=$2`, id, client).Scan(&active)
		if err != nil {
			break
		}
		if !active {
			err = calls.ErrConflict
			break
		}
		err = controlBump(ctx, tx, id, "active", "")
	case "reject":
		if !isClient || out.Direction != "incoming" || out.WinnerID != "" {
			err = calls.ErrConflict
			break
		}
		_, err = tx.ExecContext(ctx, `UPDATE call_participants SET state='ended',reason='declined',ended_at=clock_timestamp(),state_revision=state_revision+1 WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended'`, id, p.DeviceID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET candidate_client_id=NULL,candidate_until=NULL WHERE call_id=$1 AND candidate_client_id=$2`, id, p.DeviceID)
		}
		var remain bool
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_participants WHERE call_id=$1 AND state<>'ended')`, id).Scan(&remain)
		}
		if err == nil {
			if remain {
				err = controlBump(ctx, tx, id, "ringing", "")
			} else {
				err = controlRequestEnd(ctx, tx, out, "declined")
			}
		}
	case "cancel", "hangup":
		if !isClient || (out.Direction == "incoming" && out.WinnerID != p.DeviceID && out.CandidateID != p.DeviceID) {
			err = calls.ErrForbidden
			break
		}
		if body.Kind == "cancel" && out.State == "active" {
			err = calls.ErrConflict
			break
		}
		err = controlRequestEnd(ctx, tx, out, body.Kind)
	case "answered_elsewhere":
		if !isGateway || out.Direction != "incoming" || out.WinnerID != "" {
			err = calls.ErrForbidden
			break
		}
		err = controlEnd(ctx, tx, id, "answered_elsewhere")
	case "ended":
		if !isGateway {
			err = calls.ErrForbidden
			break
		}
		err = controlEnd(ctx, tx, id, "cellular_ended")
	default:
		err = calls.ErrInvalid
	}
	if err != nil {
		fail(err)
		return
	}
	out, err = controlSnapshot(ctx, tx, p, id, false)
	if err != nil {
		fail(err)
		return
	}
	if err = controlACK(ctx, tx, p, body.OperationID, body, out); err != nil {
		fail(err)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(err)
		return
	}
	s.logger.Info("RTC control committed", "call_id", id, "call_control", "rtc", "event", body.Kind, "revision", out.StateRevision)
	writeJSON(w, 200, out)
}

func (s *Server) rtcControlRead(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.requireRTCControl(w) {
		return
	}
	ctx := r.Context()
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	ids := []string{}
	if id := r.PathValue("call_id"); id != "" {
		if !validUUID(id) {
			writeCallError(w, calls.ErrInvalid)
			return
		}
		ids = append(ids, id)
	} else {
		rows, e := tx.QueryContext(ctx, `SELECT c.call_id::text FROM call_sessions c WHERE c.owner_id=$1 AND c.control_transport='rtc' AND (c.gateway_id=$2 OR c.client_device_id=$2 OR EXISTS(SELECT 1 FROM call_participants p WHERE p.call_id=c.call_id AND p.client_device_id=$2)) AND (c.state<>'ended' OR c.updated_at>clock_timestamp()-interval '15 minutes') ORDER BY (c.state<>'ended') DESC,c.updated_at DESC LIMIT 32`, p.OwnerID, p.DeviceID)
		if e != nil {
			writeDBUnavailable(w)
			return
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	results := []rtcControlSnapshot{}
	for _, id := range ids {
		out, e := controlSnapshot(ctx, tx, p, id, false)
		if e != nil {
			writeCallError(w, e)
			return
		}
		// Poll renews only this authenticated participant's control lease. It
		// cannot extend the invitation/reservation deadline or resume unknown.
		column := "client_seen_at"
		if p.Role == "gateway" {
			column = "gateway_seen_at"
		}
		if p.Role == "gateway" || out.Direction == "outgoing" || out.CandidateID == p.DeviceID || out.WinnerID == p.DeviceID {
			if _, e = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET `+column+`=clock_timestamp() WHERE call_id=$1`, id); e != nil {
				writeDBUnavailable(w)
				return
			}
		}
		results = append(results, out)
	}
	if err = tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	if r.PathValue("call_id") != "" {
		writeJSON(w, 200, results[0])
	} else {
		writeJSON(w, 200, map[string]any{"version": 1, "calls": results, "poll_after_ms": 750})
	}
}

// Lease expiry fails closed without guessing the modem result. Dispatched calls
// retain their gateway slot as unknown until the gateway confirms cellular END.
func (s *Server) sweepRTCControl(ctx context.Context) error {
	if s.rtc == nil {
		return nil
	}
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.call_id::text FROM call_sessions c JOIN rtc_control_calls t ON t.call_id=c.call_id WHERE c.control_transport='rtc' AND c.state NOT IN ('ended','unknown') AND (
      (c.state<>'active' AND c.expires_at<=clock_timestamp()) OR
      t.gateway_seen_at<clock_timestamp()-interval '20 seconds' OR
      ((c.direction='outgoing' OR c.incoming_winner_client_device_id IS NOT NULL) AND t.client_seen_at<clock_timestamp()-interval '20 seconds') OR
      EXISTS(SELECT 1 FROM devices d WHERE d.id IN(c.gateway_id,c.client_device_id) AND d.state<>'active'))
      ORDER BY c.call_id LIMIT 100 FOR UPDATE OF c,t SKIP LOCKED`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		var dispatched bool
		var direction string
		if err = tx.QueryRowContext(ctx, `SELECT t.dispatched,c.direction FROM rtc_control_calls t JOIN call_sessions c ON c.call_id=t.call_id WHERE c.call_id=$1`, id).Scan(&dispatched, &direction); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE rtc_control_calls SET end_requested=true WHERE call_id=$1`, id); err != nil {
			return err
		}
		if !dispatched && direction == "outgoing" {
			err = controlEnd(ctx, tx, id, "control_expired")
		} else {
			err = controlBump(ctx, tx, id, "unknown", "control_expired")
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EndLegacySIPCalls ends calls a SIP release left unfinished. Nothing advances
// them any more, and the cellular slot they hold would block every new call.
func (s *Server) EndLegacySIPCalls(ctx context.Context) (int64, error) {
	tx, err := beginDurable(ctx, s.db)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE call_participants p SET state='ended',reason=COALESCE(p.reason,'sip_removed'),ended_at=COALESCE(p.ended_at,clock_timestamp()),state_revision=p.state_revision+1,updated_at=clock_timestamp()
      FROM call_sessions c WHERE p.call_id=c.call_id AND c.control_transport='sip' AND c.state<>'ended' AND p.state<>'ended'`); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason=COALESCE(reason,'sip_removed'),state_revision=state_revision+1,ended_at=clock_timestamp(),updated_at=clock_timestamp()
      WHERE control_transport='sip' AND state<>'ended'`)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM gateway_call_slots s USING call_sessions c WHERE s.call_id=c.call_id AND c.control_transport='sip'`); err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return count, tx.Commit()
}
