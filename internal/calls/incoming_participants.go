package calls

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

type incomingClient struct {
	deviceID   string
	endpointID string
}

type incomingLegCleanup struct {
	clientID, channelID, wrapperID string
}

func lockIncomingDevices(ctx context.Context, tx *sql.Tx, ownerID, clientID, callID string) error {
	var gatewayID string
	if err := tx.QueryRowContext(ctx, `SELECT gateway_id::text FROM call_sessions WHERE call_id=$1 AND owner_id=$2`, callID, ownerID).Scan(&gatewayID); err != nil {
		return requireFound(err)
	}
	return lockIntentDevices(ctx, tx, ownerID, clientID, gatewayID)
}

// The participant's ended row and channel IDs are the retry ledger. Failure
// to CANCEL that leg must not disconnect a different, accepted participant.
func (m *Manager) cancelCandidateChannel(ctx context.Context, callID, clientID, channelID string) {
	if channelID == "" {
		return
	}
	if err := m.ari.Hangup(ctx, channelID); err != nil {
		var responseErr *asterisk.ResponseError
		if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
			slog.Warn("Incoming candidate cancellation will be retried", "call_id", callID, "client_id", clientID, "error", err)
		}
	}
}

func incomingRejectReason(cause int) string {
	switch cause {
	case 17:
		return "busy"
	case 21:
		return "rejected"
	default:
		return "no_answer"
	}
}

func (m *Manager) endIncomingParticipant(ctx context.Context, callID, clientID string, cause int) error {
	return m.endIncomingCandidate(ctx, callID, clientID, incomingRejectReason(cause))
}

func (m *Manager) endIncomingCandidate(ctx context.Context, callID, clientID, candidateReason string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var direction, state, winner, ownerID string
	err = tx.QueryRowContext(ctx, `
		SELECT direction,state,COALESCE(incoming_winner_client_device_id::text,''),owner_id::text
		FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).Scan(&direction, &state, &winner, &ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if direction != "incoming" || state == "ended" {
		return nil
	}
	var participantState, reason, channelID, wrapperID string
	var revision int64
	err = tx.QueryRowContext(ctx, `
		SELECT state,COALESCE(reason,''),COALESCE(client_channel_id,''),COALESCE(wrapper_channel_id,''),state_revision
		FROM call_participants WHERE call_id=$1 AND client_device_id=$2 FOR UPDATE`, callID, clientID).
		Scan(&participantState, &reason, &channelID, &wrapperID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if participantState == "ended" {
		return tx.Commit()
	}
	if winner != "" {
		if err := tx.Commit(); err != nil {
			return err
		}
		return m.endIncomingCall(ctx, callID, candidateReason)
	}
	reason = candidateReason
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_participants SET state='ended',reason=$3,ended_at=clock_timestamp(),state_revision=state_revision+1,updated_at=clock_timestamp()
		WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended'`, callID, clientID, reason); err != nil {
		return err
	}
	if err := appendParticipantEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended", reason); err != nil {
		return err
	}
	var candidates int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM call_participants WHERE call_id=$1 AND state<>'ended'`, callID).Scan(&candidates); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, id := range []string{channelID, wrapperID} {
		m.cancelCandidateChannel(ctx, callID, clientID, id)
	}
	if candidates == 0 {
		return m.endIncomingCall(ctx, callID, "no_answer")
	}
	return nil
}

// endIncomingCall leaves the gateway slot held until ARI confirms every
// channel inherited by this call is gone. Repeated terminal events are safe.
func (m *Manager) endIncomingCall(ctx context.Context, callID, reason string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerID, direction, state string
	err = tx.QueryRowContext(ctx, `SELECT owner_id::text,direction,state FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&ownerID, &direction, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if direction != "incoming" {
		return ErrConflict
	}
	if state == "ended" {
		return tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT client_device_id::text,state_revision FROM call_participants WHERE call_id=$1 AND state<>'ended' FOR UPDATE`, callID)
	if err != nil {
		return err
	}
	type participantEnd struct {
		clientID string
		revision int64
	}
	var pending []participantEnd
	for rows.Next() {
		var item participantEnd
		if err := rows.Scan(&item.clientID, &item.revision); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range pending {
		if _, err := tx.ExecContext(ctx, `
			UPDATE call_participants SET state='ended',reason=$3,ended_at=clock_timestamp(),state_revision=state_revision+1,updated_at=clock_timestamp()
			WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended'`, callID, item.clientID, reason); err != nil {
			return err
		}
		if err := appendParticipantEvent(ctx, tx, callID, ownerID, item.clientID, item.revision+1, "call.ended", "ended", reason); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_sessions SET state='unknown',reason=$2,state_revision=state_revision+CASE WHEN state='unknown' THEN 0 ELSE 1 END,
		       wake_nonce=NULL,updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, callID, reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := m.hangupKnownIncoming(ctx, callID); err != nil {
		return err
	}
	confirmedGone, err := m.stopIncomingCall(ctx, callID)
	if err != nil {
		return err
	}
	if confirmedGone {
		return m.finishCall(ctx, callID, reason)
	}
	return nil
}

func (m *Manager) hangupKnownIncoming(ctx context.Context, callID string) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(c.gateway_channel_id,''),COALESCE(c.client_channel_id,''),COALESCE(c.ari_wrapper_channel_id,''),
		       COALESCE(p.client_channel_id,''),COALESCE(p.wrapper_channel_id,'')
		FROM call_sessions c LEFT JOIN call_participants p ON p.call_id=c.call_id WHERE c.call_id=$1`, callID)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	var ids []string
	for rows.Next() {
		var gateway, client, wrapper, participantClient, participantWrapper string
		if err := rows.Scan(&gateway, &client, &wrapper, &participantClient, &participantWrapper); err != nil {
			_ = rows.Close()
			return err
		}
		for _, id := range []string{gateway, client, wrapper, participantClient, participantWrapper} {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := m.ari.Hangup(ctx, id); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) reconcileIncomingParticipants(ctx context.Context, channels []asterisk.Channel) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.call_id::text,p.client_device_id::text,p.owner_id::text,p.endpoint_id,p.state,
		       COALESCE(p.client_channel_id,''),COALESCE(p.wrapper_channel_id,''),
		       EXISTS(SELECT 1 FROM devices d WHERE d.id=p.client_device_id AND d.owner_id=p.owner_id
	         AND d.role='client' AND d.state='active'
	         AND EXISTS(SELECT 1 FROM sip_endpoint_bindings b WHERE b.device_id=d.id AND b.endpoint_id=p.endpoint_id AND b.state='active')
	         AND EXISTS(SELECT 1 FROM sessions s WHERE s.device_id=d.id
	                    AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp())))
		FROM call_participants p JOIN call_sessions c ON c.call_id=p.call_id
		WHERE c.direction='incoming' AND c.state<>'ended'`)
	if err != nil {
		return err
	}
	type candidate struct {
		callID, clientID, ownerID, endpointID, state, channelID, wrapperID string
		valid                                                              bool
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.callID, &item.clientID, &item.ownerID, &item.endpointID, &item.state, &item.channelID, &item.wrapperID, &item.valid); err != nil {
			_ = rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range candidates {
		if item.state == "ended" {
			// Retry a failed loser CANCEL without touching the winner or gateway.
			for _, channel := range channels {
				if channel.ID == item.channelID || channel.ID == item.wrapperID {
					m.cancelCandidateChannel(ctx, item.callID, item.clientID, channel.ID)
				}
			}
			continue
		}
		if item.valid {
			// Recover an Up event observed before its Stasis leg was persisted.
			if item.state == "connecting" {
				for _, channel := range channels {
					endpoint, validName := endpointFromChannelName(channel.Name)
					if channel.ID == item.channelID && validName && endpoint == item.endpointID && strings.EqualFold(channel.State, "up") {
						err := m.acceptIncomingAnswer(ctx, deviceIdentity{deviceID: item.clientID, ownerID: item.ownerID, role: "client"}, item.endpointID, channel.ID)
						if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
							return err
						}
					}
				}
			}
			continue
		}
		if err := m.endIncomingCandidate(ctx, item.callID, item.clientID, "client_endpoint_revoked"); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) stopIncomingCall(ctx context.Context, callID string) (bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(c.gateway_channel_id,''),COALESCE(c.client_channel_id,''),COALESCE(c.ari_wrapper_channel_id,''),COALESCE(c.bridge_id,''),
		       COALESCE(p.client_channel_id,''),COALESCE(p.wrapper_channel_id,'')
		FROM call_sessions c LEFT JOIN call_participants p ON p.call_id=c.call_id
		WHERE c.call_id=$1`, callID)
	if err != nil {
		return false, err
	}
	known := make(map[string]bool)
	bridgeID := ""
	for rows.Next() {
		var gateway, client, wrapper, bridge, participantChannel, participantWrapper string
		if err := rows.Scan(&gateway, &client, &wrapper, &bridge, &participantChannel, &participantWrapper); err != nil {
			_ = rows.Close()
			return false, err
		}
		for _, id := range []string{gateway, client, wrapper, participantChannel, participantWrapper} {
			if id != "" {
				known[id] = true
			}
		}
		if bridge != "" {
			bridgeID = bridge
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	_ = rows.Close()
	channels, err := m.ari.ListChannels(ctx)
	if err != nil {
		return false, err
	}
	ids := make([]string, 0, len(known))
	for id := range known {
		ids = append(ids, id)
	}
	related, complete := relatedChannels(ctx, m.ari, channels, callID, known)
	for _, channel := range related {
		if err := m.ari.Hangup(ctx, channel.ID); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				complete = false
			}
		}
	}
	if bridgeID != "" {
		if err := m.ari.DestroyBridge(ctx, bridgeID); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				complete = false
			}
		}
	}
	remaining, err := m.ari.ListChannels(ctx)
	if err != nil {
		return false, err
	}
	stillRelated, scanComplete := relatedChannels(ctx, m.ari, remaining, callID, known)
	if len(stillRelated) > 0 || !complete || !scanComplete {
		return false, nil
	}
	return true, nil
}

func activeIncomingClientsTx(ctx context.Context, tx *sql.Tx, ownerID, gatewayID string) ([]incomingClient, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id::text,d.role,b.endpoint_id,
		       EXISTS (SELECT 1 FROM sessions s WHERE s.device_id=d.id
		               AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp()))
		FROM devices d
		LEFT JOIN sip_endpoint_bindings b ON b.device_id=d.id AND b.state='active'
		WHERE (d.owner_id=$1 AND d.role='client' AND d.state='active' AND b.endpoint_id IS NOT NULL
		       AND EXISTS (SELECT 1 FROM sessions s WHERE s.device_id=d.id
		                  AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp())))
		   OR d.id=$2
		ORDER BY d.id FOR UPDATE OF d`, ownerID, gatewayID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clients []incomingClient
	var gatewayValid bool
	for rows.Next() {
		var client incomingClient
		var role string
		var endpoint sql.NullString
		var liveSession bool
		if err := rows.Scan(&client.deviceID, &role, &endpoint, &liveSession); err != nil {
			return nil, err
		}
		if client.deviceID == gatewayID {
			gatewayValid = role == "gateway" && liveSession
			continue
		}
		client.endpointID = endpoint.String
		clients = append(clients, client)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !gatewayValid {
		return nil, ErrGatewayUnavailable
	}
	if len(clients) == 0 {
		return nil, ErrNotFound
	}
	return clients, nil
}

func appendParticipantEvent(ctx context.Context, tx *sql.Tx, callID, ownerID, clientID string, revision int64, eventType, state string, reason string) error {
	body, err := json.Marshal(map[string]any{
		"call_id": callID, "event_type": eventType, "state": state,
		"state_revision": revision, "reason": reason,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO call_events(call_id,owner_id,client_device_id,state_revision,event_type,event_json)
		VALUES($1,$2,$3,$4,$5,$6)`, callID, ownerID, clientID, revision, eventType, string(body))
	return err
}

func (m *Manager) onIncomingClientLeg(ctx context.Context, identity deviceIdentity, endpointID string, channel asterisk.Channel, callID string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIncomingDevices(ctx, tx, identity.ownerID, identity.deviceID, callID); err != nil {
		return err
	}
	var ownerID, direction, state, winner string
	err = tx.QueryRowContext(ctx, `
		SELECT owner_id::text,direction,state,COALESCE(incoming_winner_client_device_id::text,'')
		FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).Scan(&ownerID, &direction, &state, &winner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ownerID != identity.ownerID || identity.role != "client" || direction != "incoming" {
		return ErrForbidden
	}
	var participantState, storedEndpoint, oldChannel, participantReason string
	var participantRevision int64
	err = tx.QueryRowContext(ctx, `
		SELECT state,endpoint_id,COALESCE(client_channel_id,''),COALESCE(reason,''),state_revision
		FROM call_participants WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3 FOR UPDATE`,
		callID, identity.ownerID, identity.deviceID).Scan(&participantState, &storedEndpoint, &oldChannel, &participantReason, &participantRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if storedEndpoint != endpointID || winner != "" && winner != identity.deviceID || state == "ended" || state == "unknown" {
		return ErrConflict
	}
	if oldChannel != "" && oldChannel != channel.ID {
		return ErrConflict
	}
	if participantState == "ended" {
		if participantReason == "answered_elsewhere" && oldChannel == channel.ID {
			return tx.Commit()
		}
		return ErrConflict
	}
	if participantState != "connecting" && participantState != "accepted" {
		if participantState != "ringing" {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE call_participants SET client_channel_id=COALESCE(client_channel_id,$3),state='connecting',
			       state_revision=state_revision+1,updated_at=clock_timestamp()
			WHERE call_id=$1 AND client_device_id=$2`, callID, identity.deviceID, channel.ID); err != nil {
			return err
		}
		if err := appendParticipantEvent(ctx, tx, callID, identity.ownerID, identity.deviceID,
			participantRevision+1, "call.connecting", "connecting", ""); err != nil {
			return err
		}
	} else if oldChannel == "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE call_participants SET client_channel_id=$3,updated_at=clock_timestamp()
			WHERE call_id=$1 AND client_device_id=$2`, callID, identity.deviceID, channel.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if strings.EqualFold(channel.State, "up") {
		return m.acceptIncomingAnswer(ctx, identity, endpointID, channel.ID)
	}
	return nil
}

// acceptIncomingAnswer commits the winner before any bridge work. The call row
// lock makes simultaneous PJSIP Up events select exactly one client across
// coordinator goroutines and processes.
func (m *Manager) acceptIncomingAnswer(ctx context.Context, identity deviceIdentity, endpointID, channelID string) error {
	if identity.role != "client" || channelID == "" {
		return ErrForbidden
	}
	var callID string
	err := m.db.QueryRowContext(ctx, `
		SELECT call_id::text FROM call_participants
		WHERE owner_id=$1 AND client_device_id=$2 AND endpoint_id=$3 AND client_channel_id=$4`,
		identity.ownerID, identity.deviceID, endpointID, channelID).Scan(&callID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIncomingDevices(ctx, tx, identity.ownerID, identity.deviceID, callID); err != nil {
		return err
	}
	var ownerID, direction, state, winner, gatewayID string
	var revision int64
	err = tx.QueryRowContext(ctx, `
		SELECT owner_id::text,direction,state,COALESCE(incoming_winner_client_device_id::text,''),gateway_id::text,state_revision
		FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&ownerID, &direction, &state, &winner, &gatewayID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ownerID != identity.ownerID || direction != "incoming" {
		return ErrForbidden
	}
	if state == "ended" || state == "unknown" {
		return ErrConflict
	}
	var participantState, participantEndpoint, participantChannel, wrapperID string
	var participantRevision int64
	err = tx.QueryRowContext(ctx, `
		SELECT state,endpoint_id,COALESCE(client_channel_id,''),COALESCE(wrapper_channel_id,''),state_revision
		FROM call_participants WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3 FOR UPDATE`,
		callID, identity.ownerID, identity.deviceID).
		Scan(&participantState, &participantEndpoint, &participantChannel, &wrapperID, &participantRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if participantEndpoint != endpointID || participantChannel != channelID {
		return ErrForbidden
	}
	if winner != "" {
		if winner != identity.deviceID {
			return ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return m.connectIfReady(ctx, callID)
	}
	if participantState != "connecting" && participantState != "ringing" {
		return ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT client_device_id::text,COALESCE(client_channel_id,''),COALESCE(wrapper_channel_id,''),state_revision
		FROM call_participants WHERE call_id=$1 AND client_device_id<>$2 AND state<>'ended' FOR UPDATE`, callID, identity.deviceID)
	if err != nil {
		return err
	}
	var losers []incomingLegCleanup
	var loserRevisions = make(map[string]int64)
	for rows.Next() {
		var loser incomingLegCleanup
		var participantRevision int64
		if err := rows.Scan(&loser.clientID, &loser.channelID, &loser.wrapperID, &participantRevision); err != nil {
			_ = rows.Close()
			return err
		}
		losers = append(losers, loser)
		loserRevisions[loser.clientID] = participantRevision
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_participants SET state='accepted',accepted_at=COALESCE(accepted_at,clock_timestamp()),
		       state_revision=state_revision+1,updated_at=clock_timestamp()
		WHERE call_id=$1 AND client_device_id=$2`, callID, identity.deviceID); err != nil {
		return err
	}
	if err := appendParticipantEvent(ctx, tx, callID, ownerID, identity.deviceID, participantRevision+1, "call.connecting", "connecting", ""); err != nil {
		return err
	}
	for _, loser := range losers {
		if _, err := tx.ExecContext(ctx, `
			UPDATE call_participants SET state='ended',reason='answered_elsewhere',ended_at=clock_timestamp(),
			       state_revision=state_revision+1,updated_at=clock_timestamp()
			WHERE call_id=$1 AND client_device_id=$2 AND state<>'ended'`, callID, loser.clientID); err != nil {
			return err
		}
		if err := appendParticipantEvent(ctx, tx, callID, ownerID, loser.clientID, loserRevisions[loser.clientID]+1,
			"call.ended", "ended", "answered_elsewhere"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_sessions SET incoming_winner_client_device_id=$2,client_device_id=$2,client_endpoint_id=$3,
		       client_channel_id=$4,ari_wrapper_channel_id=NULLIF($5,''),state='connecting',
		       state_revision=state_revision+1,updated_at=clock_timestamp()
		WHERE call_id=$1 AND incoming_winner_client_device_id IS NULL`, callID, identity.deviceID, endpointID, channelID, wrapperID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, loser := range losers {
		for _, id := range []string{loser.channelID, loser.wrapperID} {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			m.cancelCandidateChannel(ctx, callID, loser.clientID, id)
		}
	}
	return m.connectIfReady(ctx, callID)
}

func (m *Manager) clientReadyParticipant(ctx context.Context, ownerID, clientID string, item Call, wakeNonce string) (Call, error) {
	var state, storedNonce, endpointID string
	var revision int64
	err := m.db.QueryRowContext(ctx, `
		SELECT state,wake_nonce,endpoint_id,state_revision FROM call_participants
		WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3`, item.CallID, ownerID, clientID).
		Scan(&state, &storedNonce, &endpointID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrForbidden
	}
	if err != nil {
		return Call{}, err
	}
	if !equalSecret(storedNonce, wakeNonce) {
		return Call{}, ErrConflict
	}
	if state == "ringing" || state == "connecting" || state == "accepted" {
		return item, nil
	}
	if state != "pending_wakeup" && state != "candidate" {
		return Call{}, ErrConflict
	}
	if !m.Ready(ctx) {
		return Call{}, ErrNotReady
	}
	binding, err := m.loadBinding(ctx, m.db, ownerID, clientID, "client")
	if err != nil {
		return Call{}, err
	}
	if binding.endpointID != endpointID {
		return Call{}, ErrGatewayUnavailable
	}
	if online, err := m.ari.EndpointOnline(ctx, endpointID); err != nil || !online {
		return Call{}, ErrGatewayUnavailable
	}

	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return Call{}, err
	}
	defer tx.Rollback()
	// Match the device-before-call lock order used by pairing revocation and
	// credential rotation. A pending participant is not fresh authorization.
	if err := lockIntentDevices(ctx, tx, ownerID, clientID, item.GatewayID); err != nil {
		return Call{}, err
	}
	var direction, callState, gatewayEndpoint, gatewayChannel, winner string
	var expires time.Time
	var mappingRevision int64
	err = tx.QueryRowContext(ctx, `
		SELECT direction,state,gateway_endpoint_id,COALESCE(gateway_channel_id,''),
		       COALESCE(incoming_winner_client_device_id::text,''),expires_at,mapping_revision
		FROM call_sessions WHERE call_id=$1 AND owner_id=$2 FOR UPDATE`, item.CallID, ownerID).
		Scan(&direction, &callState, &gatewayEndpoint, &gatewayChannel, &winner, &expires, &mappingRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrNotFound
	}
	if err != nil {
		return Call{}, err
	}
	if direction != "incoming" || callState == "ended" || callState == "unknown" || winner != "" && winner != clientID {
		return Call{}, ErrConflict
	}
	err = tx.QueryRowContext(ctx, `
		SELECT state,wake_nonce,endpoint_id,state_revision FROM call_participants
		WHERE call_id=$1 AND owner_id=$2 AND client_device_id=$3 FOR UPDATE`, item.CallID, ownerID, clientID).
		Scan(&state, &storedNonce, &endpointID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrForbidden
	}
	if err != nil {
		return Call{}, err
	}
	if !equalSecret(storedNonce, wakeNonce) {
		return Call{}, ErrConflict
	}
	if state == "ringing" || state == "connecting" || state == "accepted" {
		if err := tx.Commit(); err != nil {
			return Call{}, err
		}
		return m.GetCall(ctx, ownerID, clientID, item.CallID)
	}
	if state != "pending_wakeup" && state != "candidate" {
		return Call{}, ErrConflict
	}
	if !expires.After(time.Now().UTC()) {
		_ = tx.Rollback()
		_ = m.endIncomingCall(ctx, item.CallID, "no_answer")
		return Call{}, ErrConflict
	}
	var currentRevision, simRevision int64
	var simState string
	var identityVerified bool
	err = tx.QueryRowContext(ctx, `
		SELECT g.mapping_revision,sb.mapping_revision,sb.state,sb.identity_verified
		FROM gateways g JOIN sim_bindings sb ON sb.gateway_id=g.device_id
		WHERE g.device_id=$1 AND sb.sim_id=$2 AND sb.owner_id=$3 FOR UPDATE OF g,sb`,
		item.GatewayID, item.SIMID, ownerID).Scan(&currentRevision, &simRevision, &simState, &identityVerified)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (currentRevision != mappingRevision || simRevision != mappingRevision || simState != "active" || !identityVerified) {
		_ = tx.Rollback()
		_ = m.endIncomingCall(ctx, item.CallID, "sim_mapping_changed")
		return Call{}, ErrSIMUnavailable
	}
	if err != nil {
		return Call{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, clientID, "client", endpointID); err != nil {
		return Call{}, err
	}
	if err := validateBindingTx(ctx, tx, ownerID, item.GatewayID, "gateway", gatewayEndpoint); err != nil {
		_ = tx.Rollback()
		_ = m.endIncomingCall(ctx, item.CallID, "gateway_unavailable")
		return Call{}, ErrGatewayUnavailable
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_participants SET state='ringing',state_revision=state_revision+1,updated_at=clock_timestamp()
		WHERE call_id=$1 AND client_device_id=$2`, item.CallID, clientID); err != nil {
		return Call{}, err
	}
	if err := appendParticipantEvent(ctx, tx, item.CallID, ownerID, clientID, revision+1, "call.ringing", "ringing", ""); err != nil {
		return Call{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_sessions SET state=CASE WHEN state='pending_wakeup' THEN 'ringing' ELSE state END,
		       state_revision=state_revision+CASE WHEN state='pending_wakeup' THEN 1 ELSE 0 END,
		       updated_at=clock_timestamp() WHERE call_id=$1`, item.CallID); err != nil {
		return Call{}, err
	}
	if err := tx.Commit(); err != nil {
		return Call{}, err
	}

	channel, err := m.ari.CreateChannel(ctx, asterisk.OriginateRequest{
		Endpoint: "Local/s@gsm2sip-ari-client/n", App: m.config.ARIApplication,
		AppArgs:  "originate-wrapper," + item.CallID + "," + clientID,
		CallerID: valueOrEmpty(item.From),
		Variables: map[string]string{
			"__GSM2SIP_ENDPOINT":  endpointID,
			"__GSM2SIP_CALL_ID":   item.CallID,
			"__GSM2SIP_CLIENT_ID": clientID,
			"__GSM2SIP_LEG_KIND":  "client-leg",
		},
	})
	if err != nil {
		_ = m.markUnknown(context.Background(), item.CallID, "ari_originate_uncertain")
		return Call{}, ErrNotReady
	}
	if channel.ID != "" {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE call_participants SET wrapper_channel_id=COALESCE(wrapper_channel_id,$3),updated_at=clock_timestamp()
			WHERE call_id=$1 AND client_device_id=$2 AND state IN ('ringing','connecting')`, item.CallID, clientID, channel.ID); err != nil {
			_ = m.markUnknown(context.Background(), item.CallID, "ari_channel_persist_failed")
			return Call{}, err
		}
	}
	return m.GetCall(ctx, ownerID, clientID, item.CallID)
}
