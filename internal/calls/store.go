package calls

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type endpointBinding struct {
	endpointID string
	aor        string
}

func (m *Manager) CreateIntent(ctx context.Context, ownerID, clientID string, request CreateIntentRequest, idempotencyKey string) (Intent, error) {
	if !m.Configured() {
		return Intent{}, ErrNotConfigured
	}
	if !validUUID(ownerID) || !validUUID(clientID) || !validUUID(request.GatewayID) || !validUUID(request.SIMID) ||
		request.MappingRevision < 1 || !validDialAddress(request.To) || len(idempotencyKey) < 1 || len(idempotencyKey) > 200 {
		return Intent{}, ErrInvalid
	}
	payloadHash, err := hashPayload(request)
	if err != nil {
		return Intent{}, err
	}
	keyHash := m.idempotencyHash(ownerID, clientID, idempotencyKey)
	if old, err := m.findIdempotent(ctx, m.db, ownerID, clientID, keyHash, payloadHash); err == nil {
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Intent{}, err
	}
	if !m.Ready(ctx) {
		if m.Configured() {
			return Intent{}, ErrNotReady
		}
		return Intent{}, ErrNotConfigured
	}

	clientBinding, err := m.loadBinding(ctx, m.db, ownerID, clientID, "client")
	if err != nil {
		return Intent{}, err
	}
	gatewayBinding, err := m.loadBinding(ctx, m.db, ownerID, request.GatewayID, "gateway")
	if err != nil {
		return Intent{}, err
	}
	if online, err := m.ari.EndpointOnline(ctx, gatewayBinding.endpointID); err != nil || !online {
		return Intent{}, ErrGatewayUnavailable
	}

	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ownerID+":"+clientID+":"+hex.EncodeToString(keyHash)); err != nil {
		return Intent{}, err
	}
	if old, err := m.findIdempotent(ctx, tx, ownerID, clientID, keyHash, payloadHash); err == nil {
		return old, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Intent{}, err
	}
	if err := lockIntentDevices(ctx, tx, ownerID, clientID, request.GatewayID); err != nil {
		return Intent{}, err
	}
	if err := m.lockAndValidateIntent(ctx, tx, ownerID, clientID, request); err != nil {
		return Intent{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, clientID, "client", clientBinding.endpointID); err != nil {
		return Intent{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, request.GatewayID, "gateway", gatewayBinding.endpointID); err != nil {
		return Intent{}, err
	}
	if err := m.releaseExpiredReservation(ctx, tx, request.GatewayID); err != nil {
		return Intent{}, err
	}
	var occupied string
	err = tx.QueryRowContext(ctx, `SELECT call_id::text FROM gateway_call_slots WHERE gateway_id=$1 FOR UPDATE`, request.GatewayID).Scan(&occupied)
	if err == nil {
		return Intent{}, ErrBusy
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Intent{}, err
	}

	intentID, err := randomUUID()
	if err != nil {
		return Intent{}, err
	}
	callID, err := randomUUID()
	if err != nil {
		return Intent{}, err
	}
	token := m.tokenFor(intentID)
	intent := Intent{IntentID: intentID, CallID: callID, SIPURI: m.sipURI(token), ExpiresAt: time.Now().UTC().Add(intentLifetime)}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO call_sessions(
		  call_id,intent_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,
		  direction,to_address,state,state_revision,expires_at,gateway_endpoint_id,client_endpoint_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,'outgoing',$8,'reserved',1,clock_timestamp()+interval '30 seconds',$9,$10)
		RETURNING expires_at`, callID, intentID, ownerID, clientID, request.GatewayID, request.SIMID,
		request.MappingRevision, request.To, gatewayBinding.endpointID, clientBinding.endpointID).
		Scan(&intent.ExpiresAt); err != nil {
		return Intent{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO call_intents(intent_id,call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,
		                         to_address,token_hash,payload_hash,state,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved',$11)`,
		intentID, callID, ownerID, clientID, request.GatewayID, request.SIMID, request.MappingRevision,
		request.To, hashToken(token), payloadHash, intent.ExpiresAt); err != nil {
		return Intent{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, request.GatewayID, callID); err != nil {
		return Intent{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO call_intent_idempotency(owner_id,client_device_id,idempotency_key_hash,payload_hash,intent_id)
		VALUES($1,$2,$3,$4,$5)`, ownerID, clientID, keyHash, payloadHash, intentID); err != nil {
		return Intent{}, err
	}
	if err := appendEvent(ctx, tx, callID, ownerID, clientID, 1, "call.reserved", "reserved"); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

// lockIntentDevices serializes intent reservation against SIP credential
// rotation, which locks one device row before checking for active calls. Lock
// both rows in UUID order so concurrent calls between the same devices cannot
// invert the order.
func lockIntentDevices(ctx context.Context, tx *sql.Tx, ownerID, clientID, gatewayID string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id::text,d.owner_id::text,d.role,d.state,
		       EXISTS(SELECT 1 FROM sessions s WHERE s.device_id=d.id
		              AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp()))
		FROM devices d WHERE d.id=$1 OR d.id=$2 ORDER BY d.id FOR UPDATE OF d`, clientID, gatewayID)
	if err != nil {
		return err
	}
	defer rows.Close()
	foundClient, foundGateway := false, false
	for rows.Next() {
		var id, deviceOwner, role, state string
		var liveSession bool
		if err := rows.Scan(&id, &deviceOwner, &role, &state, &liveSession); err != nil {
			return err
		}
		switch id {
		case clientID:
			foundClient = deviceOwner == ownerID && role == "client" && state == "active" && liveSession
		case gatewayID:
			foundGateway = deviceOwner == ownerID && role == "gateway" && state == "active" && liveSession
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !foundClient {
		return ErrForbidden
	}
	if !foundGateway {
		return ErrGatewayUnavailable
	}
	return nil
}

func (m *Manager) findIdempotent(ctx context.Context, q queryer, ownerID, clientID string, keyHash, payloadHash []byte) (Intent, error) {
	var storedHash []byte
	var intentID, callID string
	var expiresAt time.Time
	err := q.QueryRowContext(ctx, `
		SELECT i.payload_hash,i.intent_id::text,c.call_id::text,ci.expires_at
		FROM call_intent_idempotency i JOIN call_intents ci ON ci.intent_id=i.intent_id
		JOIN call_sessions c ON c.call_id=ci.call_id
		WHERE i.owner_id=$1 AND i.client_device_id=$2 AND i.idempotency_key_hash=$3`,
		ownerID, clientID, keyHash).Scan(&storedHash, &intentID, &callID, &expiresAt)
	if err != nil {
		return Intent{}, err
	}
	if !equalBytes(storedHash, payloadHash) {
		return Intent{}, ErrConflict
	}
	return Intent{IntentID: intentID, CallID: callID, SIPURI: m.sipURI(m.tokenFor(intentID)), ExpiresAt: expiresAt}, nil
}

func (m *Manager) sipURI(token string) string {
	return "sips:call." + token + "@" + m.config.SIPRealm + ";transport=tls"
}

func (m *Manager) loadBinding(ctx context.Context, q queryer, ownerID, deviceID, role string) (endpointBinding, error) {
	var result endpointBinding
	err := q.QueryRowContext(ctx, `
		SELECT b.endpoint_id,b.aor
		FROM sip_endpoint_bindings b JOIN devices d ON d.id=b.device_id
		WHERE d.id=$1 AND d.owner_id=$2 AND d.role=$3 AND d.state='active' AND b.state='active'`,
		deviceID, ownerID, role).Scan(&result.endpointID, &result.aor)
	if errors.Is(err, sql.ErrNoRows) {
		return endpointBinding{}, ErrGatewayUnavailable
	}
	return result, err
}

func validateBindingTx(ctx context.Context, tx *sql.Tx, ownerID, deviceID, role, expectedEndpoint string) error {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
		 SELECT 1 FROM sip_endpoint_bindings b JOIN devices d ON d.id=b.device_id
		 WHERE d.id=$1 AND d.owner_id=$2 AND d.role=$3 AND d.state='active'
		   AND b.state='active' AND b.endpoint_id=$4
		)`, deviceID, ownerID, role, expectedEndpoint).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrGatewayUnavailable
	}
	return nil
}

func (m *Manager) lockAndValidateIntent(ctx context.Context, tx *sql.Tx, ownerID, clientID string, request CreateIntentRequest) error {
	var clientActive bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1 AND owner_id=$2 AND role='client' AND state='active')`, clientID, ownerID).Scan(&clientActive); err != nil {
		return err
	}
	if !clientActive {
		return ErrForbidden
	}
	var currentRevision int64
	var online, sipRegistered, simVerified bool
	var simState string
	var simRevision int64
	err := tx.QueryRowContext(ctx, `
		SELECT g.mapping_revision,
		       (g.last_seen_at IS NOT NULL AND g.last_seen_at > clock_timestamp()-interval '90 seconds'),
		       COALESCE(g.sip_registered,false),sb.state,sb.identity_verified,sb.mapping_revision
		FROM gateways g JOIN devices d ON d.id=g.device_id
		JOIN sim_bindings sb ON sb.gateway_id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.role='gateway' AND d.state='active'
		  AND sb.sim_id=$3 AND sb.owner_id=$2 FOR UPDATE OF g,sb`, request.GatewayID, ownerID, request.SIMID).
		Scan(&currentRevision, &online, &sipRegistered, &simState, &simVerified, &simRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSIMUnavailable
	}
	if err != nil {
		return err
	}
	if currentRevision != request.MappingRevision || simRevision != request.MappingRevision || simState != "active" || !simVerified {
		return ErrSIMUnavailable
	}
	if !online || !sipRegistered {
		return ErrGatewayUnavailable
	}
	return nil
}

func (m *Manager) releaseExpiredReservation(ctx context.Context, tx *sql.Tx, gatewayID string) error {
	var callID, state string
	var expiresAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT c.call_id::text,c.state,c.expires_at FROM gateway_call_slots s
		JOIN call_sessions c ON c.call_id=s.call_id WHERE s.gateway_id=$1 FOR UPDATE OF s,c`, gatewayID).
		Scan(&callID, &state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "reserved" || !expiresAt.Valid || expiresAt.Time.After(time.Now().UTC()) {
		return nil
	}
	var ownerID, clientID string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id::text,client_device_id::text,state_revision FROM call_sessions WHERE call_id=$1`, callID).
		Scan(&ownerID, &clientID, &revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='expired',updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason='intent_expired',state_revision=state_revision+1,ended_at=clock_timestamp(),updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID); err != nil {
		return err
	}
	return appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended")
}

func beginDurable(ctx context.Context, database *sql.DB) (*sql.Tx, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, callID, ownerID, clientID string, revision int64, eventType, state string) error {
	body, err := json.Marshal(map[string]any{
		"call_id": callID, "event_type": eventType, "state": state, "state_revision": revision,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO call_events(call_id,owner_id,client_device_id,state_revision,event_type,event_json)
		VALUES($1,$2,$3,$4,$5,$6)`, callID, ownerID, clientID, revision, eventType, string(body))
	return err
}

func (m *Manager) CancelIntent(ctx context.Context, ownerID, clientID, intentID string) error {
	if !validUUID(ownerID) || !validUUID(clientID) || !validUUID(intentID) {
		return ErrInvalid
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var callID, state, callState, gatewayID string
	var revision int64
	err = tx.QueryRowContext(ctx, `
		SELECT i.call_id::text,i.state,c.state,c.gateway_id::text,c.state_revision
		FROM call_intents i JOIN call_sessions c ON c.call_id=i.call_id
		WHERE i.intent_id=$1 AND i.owner_id=$2 AND i.client_device_id=$3 FOR UPDATE OF i,c`,
		intentID, ownerID, clientID).Scan(&callID, &state, &callState, &gatewayID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		var ownedElsewhere bool
		if qErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_intents WHERE intent_id=$1 AND owner_id=$2)`, intentID, ownerID).Scan(&ownedElsewhere); qErr != nil {
			return qErr
		}
		if ownedElsewhere {
			return ErrForbidden
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "cancelled" || state == "expired" {
		return tx.Commit()
	}
	if state != "reserved" || callState != "reserved" {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='cancelled',updated_at=clock_timestamp() WHERE intent_id=$1`, intentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason='cancelled',state_revision=state_revision+1,ended_at=clock_timestamp(),updated_at=clock_timestamp() WHERE call_id=$1`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended"); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) ListCalls(ctx context.Context, ownerID, clientID, cursor string, limit int) ([]Call, string, error) {
	if !validUUID(ownerID) || !validUUID(clientID) {
		return nil, "", ErrInvalid
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	createdAt, callID, err := parseCursor(cursor)
	if err != nil {
		return nil, "", ErrInvalid
	}
	query := `
		SELECT c.call_id::text,c.gateway_id::text,
		       COALESCE(p.client_device_id,c.client_device_id)::text,c.sim_id::text,c.mapping_revision,c.direction,
	       CASE WHEN p.client_device_id IS NOT NULL AND c.state='unknown' AND p.state<>'ended' THEN 'unknown'
	            WHEN p.client_device_id IS NULL THEN c.state
		            WHEN p.state='candidate' THEN 'pending_wakeup'
		            WHEN p.state='accepted' AND c.state='active' THEN 'active'
		            WHEN p.state='accepted' THEN 'connecting'
		            ELSE p.state END,
		       COALESCE(p.state_revision,c.state_revision),c.from_address,c.to_address,
		       CASE WHEN p.client_device_id IS NULL OR c.state='unknown' AND p.state<>'ended' THEN c.reason ELSE p.reason END,c.created_at,c.expires_at,
	       CASE WHEN p.state='pending_wakeup' AND c.state<>'unknown' AND c.expires_at>clock_timestamp() THEN p.wake_nonce
		            WHEN p.client_device_id IS NULL AND c.direction='incoming' AND c.state='pending_wakeup' AND c.expires_at>clock_timestamp() THEN c.wake_nonce
		            ELSE NULL END
		FROM call_sessions c LEFT JOIN call_participants p
		  ON p.call_id=c.call_id AND p.owner_id=c.owner_id AND p.client_device_id=$2
		WHERE c.owner_id=$1 AND (c.client_device_id=$2 OR p.client_device_id=$2)`
	args := []any{ownerID, clientID}
	if !createdAt.IsZero() {
		query += ` AND (c.created_at,c.call_id)<($3,$4)`
		args = append(args, createdAt, callID)
	}
	args = append(args, limit+1)
	query += ` ORDER BY c.created_at DESC,c.call_id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]Call, 0, limit+1)
	for rows.Next() {
		item, err := scanCall(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	return items, makeCursor(items[len(items)-1]), nil
}

func (m *Manager) GetCall(ctx context.Context, ownerID, clientID, callID string) (Call, error) {
	if !validUUID(ownerID) || !validUUID(clientID) || !validUUID(callID) {
		return Call{}, ErrInvalid
	}
	item, err := loadCall(ctx, m.db, ownerID, clientID, callID)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Call{}, err
	}
	var inOwner bool
	if err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_sessions WHERE call_id=$1 AND owner_id=$2)`, callID, ownerID).Scan(&inOwner); err != nil {
		return Call{}, err
	}
	if inOwner {
		return Call{}, ErrForbidden
	}
	return Call{}, ErrNotFound
}

func loadCall(ctx context.Context, q queryer, ownerID, clientID, callID string) (Call, error) {
	return scanCall(q.QueryRowContext(ctx, `
		SELECT c.call_id::text,c.gateway_id::text,
		       COALESCE(p.client_device_id,c.client_device_id)::text,c.sim_id::text,c.mapping_revision,c.direction,
	       CASE WHEN p.client_device_id IS NOT NULL AND c.state='unknown' AND p.state<>'ended' THEN 'unknown'
	            WHEN p.client_device_id IS NULL THEN c.state
		            WHEN p.state='candidate' THEN 'pending_wakeup'
		            WHEN p.state='accepted' AND c.state='active' THEN 'active'
		            WHEN p.state='accepted' THEN 'connecting'
		            ELSE p.state END,
		       COALESCE(p.state_revision,c.state_revision),c.from_address,c.to_address,
		       CASE WHEN p.client_device_id IS NULL OR c.state='unknown' AND p.state<>'ended' THEN c.reason ELSE p.reason END,c.created_at,c.expires_at,
	       CASE WHEN p.state='pending_wakeup' AND c.state<>'unknown' AND c.expires_at>clock_timestamp() THEN p.wake_nonce
		            WHEN p.client_device_id IS NULL AND c.direction='incoming' AND c.state='pending_wakeup' AND c.expires_at>clock_timestamp() THEN c.wake_nonce
		            ELSE NULL END
		FROM call_sessions c LEFT JOIN call_participants p
		  ON p.call_id=c.call_id AND p.owner_id=c.owner_id AND p.client_device_id=$3
		WHERE c.call_id=$1 AND c.owner_id=$2 AND (c.client_device_id=$3 OR p.client_device_id=$3)`, callID, ownerID, clientID))
}

type rowScanner interface{ Scan(...any) error }

func scanCall(row rowScanner) (Call, error) {
	var result Call
	var from, reason, wake sql.NullString
	var expires sql.NullTime
	err := row.Scan(&result.CallID, &result.GatewayID, &result.ClientID, &result.SIMID, &result.MappingRevision,
		&result.Direction, &result.State, &result.StateRevision, &from, &result.To, &reason, &result.CreatedAt, &expires, &wake)
	if err != nil {
		return Call{}, err
	}
	if from.Valid {
		result.From = &from.String
	}
	if reason.Valid {
		result.Reason = &reason.String
	}
	if expires.Valid {
		result.ExpiresAt = &expires.Time
	}
	if wake.Valid && wake.String != "" {
		result.WakeNonce = &wake.String
	}
	return result, nil
}

func (m *Manager) ClientReady(ctx context.Context, ownerID, clientID, pathClientID, callID, wakeNonce string) (Call, error) {
	if !validUUID(ownerID) || !validUUID(clientID) || !validUUID(pathClientID) || !validUUID(callID) || wakeNonce == "" || len(wakeNonce) > 256 {
		return Call{}, ErrInvalid
	}
	if clientID != pathClientID {
		return Call{}, ErrForbidden
	}
	item, err := m.GetCall(ctx, ownerID, clientID, callID)
	if err != nil {
		return Call{}, err
	}
	if item.Direction == "incoming" {
		var participantExists bool
		if err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_participants WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3)`, callID, ownerID, clientID).Scan(&participantExists); err != nil {
			return Call{}, err
		}
		if participantExists {
			return m.clientReadyParticipant(ctx, ownerID, clientID, item, wakeNonce)
		}
	}
	// A duplicate /ready is a replay of the same nonce, not a new originate
	// request. Return the durable result even when ARI or the SIP contact has
	// since gone away.
	if item.State == "ringing" || item.State == "connecting" || item.State == "active" {
		var storedNonce string
		if err := m.db.QueryRowContext(ctx, `SELECT wake_nonce FROM call_sessions WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3`, callID, ownerID, clientID).Scan(&storedNonce); err != nil {
			return Call{}, requireFound(err)
		}
		if !equalSecret(storedNonce, wakeNonce) {
			return Call{}, ErrConflict
		}
		return item, nil
	}
	if !m.Ready(ctx) {
		return Call{}, ErrNotReady
	}
	binding, err := m.loadBinding(ctx, m.db, ownerID, clientID, "client")
	if err != nil {
		return Call{}, err
	}
	if online, err := m.ari.EndpointOnline(ctx, binding.endpointID); err != nil || !online {
		return Call{}, ErrGatewayUnavailable
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return Call{}, err
	}
	defer tx.Rollback()
	if err := lockIntentDevices(ctx, tx, ownerID, clientID, item.GatewayID); err != nil {
		return Call{}, err
	}
	var state, storedNonce, clientEndpoint, gatewayEndpoint, gatewayChannel string
	var stateRevision int64
	var expires time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT state,wake_nonce,client_endpoint_id,gateway_endpoint_id,state_revision,expires_at,COALESCE(gateway_channel_id,'')
		FROM call_sessions WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3 FOR UPDATE`,
		callID, ownerID, clientID).Scan(&state, &storedNonce, &clientEndpoint, &gatewayEndpoint, &stateRevision, &expires, &gatewayChannel)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrNotFound
	}
	if err != nil {
		return Call{}, err
	}
	if !equalSecret(storedNonce, wakeNonce) {
		return Call{}, ErrConflict
	}
	if state == "ringing" || state == "connecting" || state == "active" {
		if err := tx.Commit(); err != nil {
			return Call{}, err
		}
		return m.GetCall(ctx, ownerID, clientID, callID)
	}
	if state != "pending_wakeup" {
		return Call{}, ErrConflict
	}
	if !expires.After(time.Now().UTC()) {
		return Call{}, m.endPendingIncoming(ctx, tx, callID, ownerID, clientID, item.GatewayID, gatewayChannel, stateRevision, "no_answer", ErrConflict)
	}
	var mappingRevision, simRevision int64
	var simState string
	var identityVerified bool
	err = tx.QueryRowContext(ctx, `
		SELECT g.mapping_revision,sb.mapping_revision,sb.state,sb.identity_verified
		FROM gateways g JOIN sim_bindings sb ON sb.gateway_id=g.device_id
		WHERE g.device_id=$1 AND sb.sim_id=$2 AND sb.owner_id=$3 FOR UPDATE OF g,sb`,
		item.GatewayID, item.SIMID, ownerID).Scan(&mappingRevision, &simRevision, &simState, &identityVerified)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (mappingRevision != item.MappingRevision || simRevision != item.MappingRevision || simState != "active" || !identityVerified) {
		return Call{}, m.endPendingIncoming(ctx, tx, callID, ownerID, clientID, item.GatewayID, gatewayChannel, stateRevision, "sim_mapping_changed", ErrSIMUnavailable)
	}
	if err != nil {
		return Call{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, clientID, "client", clientEndpoint); err != nil {
		return Call{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, item.GatewayID, "gateway", gatewayEndpoint); err != nil {
		return Call{}, m.endPendingIncoming(ctx, tx, callID, ownerID, clientID, item.GatewayID, gatewayChannel, stateRevision, "gateway_unavailable", ErrGatewayUnavailable)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ringing',state_revision=state_revision+1,updated_at=clock_timestamp() WHERE call_id=$1 AND state='pending_wakeup'`, callID); err != nil {
		return Call{}, err
	}
	if err := appendEvent(ctx, tx, callID, ownerID, clientID, stateRevision+1, "call.ringing", "ringing"); err != nil {
		return Call{}, err
	}
	if err := tx.Commit(); err != nil {
		return Call{}, err
	}

	channel, err := m.ari.CreateChannel(ctx, asterisk.OriginateRequest{
		Endpoint: "Local/s@gsm2sip-ari-client/n", App: m.config.ARIApplication,
		AppArgs:  "originate-wrapper," + callID,
		CallerID: valueOrEmpty(item.From),
		Variables: map[string]string{
			"__GSM2SIP_ENDPOINT": clientEndpoint,
			"__GSM2SIP_CALL_ID":  callID,
			"__GSM2SIP_LEG_KIND": "client-leg",
		},
	})
	if err != nil {
		_ = m.markUnknown(context.Background(), callID, "ari_originate_uncertain")
		return Call{}, ErrNotReady
	}
	if channel.ID != "" {
		if _, err := m.db.ExecContext(ctx, `UPDATE call_sessions SET ari_wrapper_channel_id=COALESCE(ari_wrapper_channel_id,$2),updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, callID, channel.ID); err != nil {
			_ = m.markUnknown(context.Background(), callID, "ari_channel_persist_failed")
			return Call{}, err
		}
	}
	return m.GetCall(ctx, ownerID, clientID, callID)
}

// endPendingIncoming hangs up the still ringing gateway leg before freeing its
// lease. If ARI cannot confirm the hangup, retain the lease and mark the call
// unknown so reconciliation can safely retry cleanup.
func (m *Manager) endPendingIncoming(ctx context.Context, tx *sql.Tx, callID, ownerID, clientID, gatewayID, gatewayChannel string, revision int64, reason string, result error) error {
	if gatewayChannel != "" {
		if err := m.ari.Hangup(ctx, gatewayChannel); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				if _, updateErr := tx.ExecContext(ctx, `UPDATE call_sessions SET state='unknown',reason='ari_hangup_uncertain',state_revision=state_revision+1,wake_nonce=NULL,updated_at=clock_timestamp() WHERE call_id=$1 AND state='pending_wakeup'`, callID); updateErr != nil {
					return updateErr
				}
				if err := appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.unknown", "unknown"); err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				return ErrGatewayUnavailable
			}
		}
	}
	if err := m.endCallTx(ctx, tx, callID, ownerID, clientID, gatewayID, revision, reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return result
}

func (m *Manager) endCallTx(ctx context.Context, tx *sql.Tx, callID, ownerID, clientID, gatewayID string, revision int64, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason=$2,state_revision=state_revision+1,ended_at=clock_timestamp(),updated_at=clock_timestamp(),wake_nonce=NULL WHERE call_id=$1 AND state<>'ended'`, callID, reason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='expired',updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID); err != nil {
		return err
	}
	return appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended")
}

func (m *Manager) markUnknown(ctx context.Context, callID, reason string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerID, clientID, direction, state string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT owner_id::text,client_device_id::text,direction,state,state_revision FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&ownerID, &clientID, &direction, &state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "ended" || state == "unknown" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='unknown',reason=$2,state_revision=state_revision+1,wake_nonce=NULL,updated_at=clock_timestamp() WHERE call_id=$1`, callID, reason); err != nil {
		return err
	}
	if direction == "incoming" {
		rows, err := tx.QueryContext(ctx, `SELECT client_device_id::text,state_revision FROM call_participants WHERE call_id=$1 AND state<>'ended' FOR UPDATE`, callID)
		if err != nil {
			return err
		}
		type participantState struct {
			clientID string
			revision int64
		}
		var participants []participantState
		for rows.Next() {
			var participant participantState
			if err := rows.Scan(&participant.clientID, &participant.revision); err != nil {
				_ = rows.Close()
				return err
			}
			participants = append(participants, participant)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		for _, participant := range participants {
			if _, err := tx.ExecContext(ctx, `UPDATE call_participants SET state_revision=state_revision+1,updated_at=clock_timestamp() WHERE call_id=$1 AND client_device_id=$2`, callID, participant.clientID); err != nil {
				return err
			}
			if err := appendParticipantEvent(ctx, tx, callID, ownerID, participant.clientID, participant.revision+1, "call.unknown", "unknown", reason); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err := appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.unknown", "unknown"); err != nil {
		return err
	}
	return tx.Commit()
}

func equalSecret(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(strings.ToLower(value), "-", "")
	_, err := hex.DecodeString(compact)
	return err == nil
}

func validDialAddress(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !(char == '+' || char == '*' || char == '#' || char == '.' || char == '(' || char == ')' || char == '-' || char == ' ' ||
			(char >= '0' && char <= '9') || (char >= 'A' && char <= 'D') || (char >= 'a' && char <= 'd')) {
			return false
		}
	}
	return true
}

func parseCursor(value string) (time.Time, string, error) {
	if value == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 || !validUUID(parts[1]) {
		return time.Time{}, "", ErrInvalid
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", ErrInvalid
	}
	return createdAt.UTC(), strings.ToLower(parts[1]), nil
}

func makeCursor(item Call) string {
	return base64.RawURLEncoding.EncodeToString([]byte(item.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + strings.ToLower(item.CallID)))
}

func requireFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func conflictIfNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	return err
}

func debugError(err error) error { return fmt.Errorf("call operation: %w", err) }
