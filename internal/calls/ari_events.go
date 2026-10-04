package calls

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

var pjsipChannelName = regexp.MustCompile(`^PJSIP/(dev_[0-9a-f]{32})-[0-9a-fA-F]{8}$`)

type deviceIdentity struct {
	deviceID string
	ownerID  string
	role     string
}

type callLegs struct {
	callID          string
	ownerID         string
	clientID        string
	gatewayID       string
	direction       string
	state           string
	stateRevision   int64
	clientEndpoint  string
	gatewayEndpoint string
	clientChannel   string
	gatewayChannel  string
	wrapperChannel  string
	bridgeID        string
}

type reconcileSession struct {
	callID, ownerID, clientID, gatewayID, state             string
	clientChannel, gatewayChannel, wrapperChannel, bridgeID string
	clientValid, gatewayValid                               bool
}

func (m *Manager) handleEvent(ctx context.Context, event asterisk.Event) error {
	switch event.Type {
	case "StasisStart":
		if event.Application != m.config.ARIApplication {
			return nil
		}
		if event.Channel.ID == "" {
			return errors.New("ARI StasisStart omitted channel ID")
		}
		if len(event.Args) == 0 {
			return m.ari.Hangup(ctx, event.Channel.ID)
		}
		if event.Args[0] == "originate-wrapper" {
			if len(event.Args) != 2 || !validUUID(event.Args[1]) {
				return m.ari.Hangup(ctx, event.Channel.ID)
			}
			if err := m.dialWrapper(ctx, event.Channel.ID, strings.ToLower(event.Args[1])); err != nil {
				_ = m.ari.Hangup(ctx, event.Channel.ID)
				if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
					return nil
				}
				return err
			}
			return nil
		}
		endpointID, ok := endpointFromChannelName(event.Channel.Name)
		if !ok {
			return m.ari.Hangup(ctx, event.Channel.ID)
		}
		switch event.Args[0] {
		case "client-intent":
			if len(event.Args) != 2 {
				return m.ari.Hangup(ctx, event.Channel.ID)
			}
			if err := m.consumeIntent(ctx, endpointID, event.Channel, event.Args[1]); err != nil {
				_ = m.ari.Hangup(ctx, event.Channel.ID)
				if errors.Is(err, ErrBusy) || errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) ||
					errors.Is(err, ErrInvalid) || errors.Is(err, ErrSIMUnavailable) || errors.Is(err, ErrGatewayUnavailable) {
					return nil
				}
				return err
			}
		case "gateway-inbound":
			if len(event.Args) != 5 || event.Args[4] != "1" {
				return m.ari.Hangup(ctx, event.Channel.ID)
			}
			revision, err := strconv.ParseInt(event.Args[3], 10, 64)
			if err != nil {
				return m.ari.Hangup(ctx, event.Channel.ID)
			}
			if err := m.acceptIncoming(ctx, endpointID, event.Channel, event.Args[1], event.Args[2], revision); err != nil {
				_ = m.ari.Hangup(ctx, event.Channel.ID)
				if errors.Is(err, ErrBusy) || errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) ||
					errors.Is(err, ErrSIMUnavailable) || errors.Is(err, ErrGatewayUnavailable) || errors.Is(err, ErrInvalid) {
					return nil
				}
				return err
			}
		case "client-leg", "gateway-leg":
			if len(event.Args) != 2 || !validUUID(event.Args[1]) {
				return m.ari.Hangup(ctx, event.Channel.ID)
			}
			if err := m.onCallLeg(ctx, endpointID, event.Channel, event.Args[0], strings.ToLower(event.Args[1])); err != nil {
				_ = m.ari.Hangup(ctx, event.Channel.ID)
				if errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
					return nil
				}
				return err
			}
		default:
			return m.ari.Hangup(ctx, event.Channel.ID)
		}
	case "ChannelDestroyed":
		return m.onChannelDestroyed(ctx, event.Channel.ID, event.Cause, event.CauseText)
	}
	return nil
}

func (m *Manager) dialWrapper(ctx context.Context, channelID, callID string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, direction string
	var expires time.Time
	var oldChannel string
	var dialStarted bool
	err = tx.QueryRowContext(ctx, `SELECT state,direction,expires_at,COALESCE(ari_wrapper_channel_id,''),ari_wrapper_dial_started FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&state, &direction, &expires, &oldChannel, &dialStarted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "ended" || state == "unknown" {
		return ErrConflict
	}
	if oldChannel != "" && oldChannel != channelID {
		return ErrConflict
	}
	if dialStarted {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET ari_wrapper_channel_id=$2,ari_wrapper_dial_started=true,updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, callID, channelID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	timeout := time.Minute
	if direction == "incoming" {
		timeout = time.Until(expires)
	}
	if err := m.ari.Dial(ctx, channelID, timeout); err != nil {
		_ = m.markUnknown(context.Background(), callID, "ari_wrapper_dial_uncertain")
		return err
	}
	return nil
}

func endpointFromChannelName(name string) (string, bool) {
	match := pjsipChannelName.FindStringSubmatch(name)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func (m *Manager) identityForEndpoint(ctx context.Context, endpointID string) (deviceIdentity, error) {
	var identity deviceIdentity
	err := m.db.QueryRowContext(ctx, `
		SELECT d.id::text,d.owner_id::text,d.role
		FROM sip_endpoint_bindings b JOIN devices d ON d.id=b.device_id
		WHERE b.endpoint_id=$1 AND b.state='active' AND d.state='active'`, endpointID).
		Scan(&identity.deviceID, &identity.ownerID, &identity.role)
	if errors.Is(err, sql.ErrNoRows) {
		return deviceIdentity{}, ErrForbidden
	}
	return identity, err
}

func (m *Manager) consumeIntent(ctx context.Context, endpointID string, channel asterisk.Channel, token string) error {
	if len(token) < 32 || len(token) > 128 || strings.ContainsAny(token, ".@/\r\n") {
		return ErrInvalid
	}
	identity, err := m.identityForEndpoint(ctx, endpointID)
	if err != nil || identity.role != "client" {
		return ErrForbidden
	}
	decoded, err := base64Token(token)
	if err != nil || len(decoded) != 32 {
		return ErrNotFound
	}
	tokenHash := hashToken(token)
	var tokenOwner, tokenClient, tokenGateway string
	err = m.db.QueryRowContext(ctx, `SELECT owner_id::text,client_device_id::text,gateway_id::text FROM call_intents WHERE token_hash=$1`, tokenHash).
		Scan(&tokenOwner, &tokenClient, &tokenGateway)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tokenOwner != identity.ownerID || tokenClient != identity.deviceID {
		return ErrForbidden
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIntentDevices(ctx, tx, identity.ownerID, identity.deviceID, tokenGateway); err != nil {
		return err
	}
	var intentID, callID, ownerID, clientID, gatewayID, simID string
	var state, callState, toAddress, gatewayEndpoint, clientEndpoint string
	var mappingRevision, stateRevision int64
	var expires time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT i.intent_id::text,i.call_id::text,i.owner_id::text,i.client_device_id::text,i.gateway_id::text,
		       i.sim_id::text,i.state,i.to_address,i.mapping_revision,i.expires_at,
	       s.state,s.state_revision,s.gateway_endpoint_id,s.client_endpoint_id
		FROM call_intents i JOIN call_sessions s ON s.call_id=i.call_id
		WHERE i.token_hash=$1 FOR UPDATE OF i,s`, tokenHash).
		Scan(&intentID, &callID, &ownerID, &clientID, &gatewayID, &simID, &state, &toAddress,
			&mappingRevision, &expires, &callState, &stateRevision, &gatewayEndpoint, &clientEndpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if clientID != identity.deviceID || ownerID != identity.ownerID || clientEndpoint != endpointID {
		return ErrForbidden
	}
	if state != "reserved" || callState != "reserved" || !expires.After(time.Now().UTC()) {
		return ErrConflict
	}
	var currentRevision, simRevision int64
	var simState string
	var simVerified, gatewayOnline, sipRegistered bool
	err = tx.QueryRowContext(ctx, `
		SELECT g.mapping_revision,
		       (g.last_seen_at IS NOT NULL AND g.last_seen_at>clock_timestamp()-interval '90 seconds'),
		       COALESCE(g.sip_registered,false),sb.mapping_revision,sb.state,sb.identity_verified
		FROM gateways g JOIN devices d ON d.id=g.device_id
		JOIN sim_bindings sb ON sb.gateway_id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.state='active'
		  AND sb.sim_id=$3 AND sb.owner_id=$2 FOR UPDATE OF g,sb`, gatewayID, ownerID, simID).
		Scan(&currentRevision, &gatewayOnline, &sipRegistered, &simRevision, &simState, &simVerified)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (currentRevision != mappingRevision || simRevision != mappingRevision || simState != "active" || !simVerified) {
		if err := cancelReservationTx(ctx, tx, intentID, callID, ownerID, clientID, gatewayID, stateRevision, "sim_mapping_changed"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrSIMUnavailable
	}
	if err != nil {
		return err
	}
	if !gatewayOnline || !sipRegistered {
		return ErrGatewayUnavailable
	}
	if err := validateBindingTx(ctx, tx, ownerID, clientID, "client", clientEndpoint); err != nil {
		return err
	}
	if err := validateBindingTx(ctx, tx, ownerID, gatewayID, "gateway", gatewayEndpoint); err != nil {
		return err
	}
	if online, err := m.ari.EndpointOnline(ctx, gatewayEndpoint); err != nil || !online {
		return ErrGatewayUnavailable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='consumed',consumed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE intent_id=$1`, intentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_sessions SET state='dialing',client_channel_id=$2,state_revision=state_revision+1,updated_at=clock_timestamp()
		WHERE call_id=$1 AND state='reserved'`, callID, channel.ID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, callID, ownerID, clientID, stateRevision+1, "call.dialing", "dialing"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	originated, err := m.ari.CreateChannel(ctx, asterisk.OriginateRequest{
		Endpoint: "Local/s@gsm2sip-ari-gateway/n",
		App:      m.config.ARIApplication,
		AppArgs:  "originate-wrapper," + callID,
		Variables: map[string]string{
			"__GSM2SIP_ENDPOINT":         gatewayEndpoint,
			"__GSM2SIP_CALL_ID":          callID,
			"__GSM2SIP_DESTINATION":      toAddress,
			"__GSM2SIP_SIM_ID":           simID,
			"__GSM2SIP_MAPPING_REVISION": strconv.FormatInt(mappingRevision, 10),
			"__GSM2SIP_PROTOCOL_VERSION": "1",
			"__GSM2SIP_LEG_KIND":         "gateway-leg",
		},
	})
	if err != nil {
		_ = m.markUnknown(context.Background(), callID, "ari_originate_uncertain")
		return err
	}
	if originated.ID != "" {
		_, err = m.db.ExecContext(ctx, `UPDATE call_sessions SET ari_wrapper_channel_id=COALESCE(ari_wrapper_channel_id,$2),updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, callID, originated.ID)
		if err != nil {
			_ = m.markUnknown(context.Background(), callID, "ari_channel_persist_failed")
			return err
		}
	}
	return nil
}

func cancelReservationTx(ctx context.Context, tx *sql.Tx, intentID, callID, ownerID, clientID, gatewayID string, revision int64, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='cancelled',updated_at=clock_timestamp() WHERE intent_id=$1 AND state='reserved'`, intentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_sessions SET state='ended',reason=$2,state_revision=state_revision+1,
		       ended_at=clock_timestamp(),updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID, reason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID); err != nil {
		return err
	}
	return appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended")
}

func base64Token(token string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(token)
}

func (m *Manager) acceptIncoming(ctx context.Context, endpointID string, channel asterisk.Channel, callID, simID string, revision int64) error {
	callID, simID = strings.ToLower(callID), strings.ToLower(simID)
	if !validUUID(callID) || !validUUID(simID) || revision < 1 {
		return ErrInvalid
	}
	identity, err := m.identityForEndpoint(ctx, endpointID)
	if err != nil || identity.role != "gateway" {
		return ErrForbidden
	}
	client, err := m.loadPrimaryClient(ctx, identity.ownerID)
	if err != nil {
		return err
	}
	wakeNonce, err := m.makeWakeNonce(callID)
	if err != nil {
		return err
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIntentDevices(ctx, tx, identity.ownerID, client.deviceID, identity.deviceID); err != nil {
		return err
	}
	var existingGateway, existingSIM, existingChannel string
	err = tx.QueryRowContext(ctx, `SELECT gateway_id::text,sim_id::text,COALESCE(gateway_channel_id,'') FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&existingGateway, &existingSIM, &existingChannel)
	if err == nil {
		if existingGateway == identity.deviceID && existingSIM == simID && existingChannel == channel.ID {
			return tx.Commit()
		}
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var currentRevision, simRevision int64
	var simState string
	var simVerified bool
	err = tx.QueryRowContext(ctx, `
		SELECT g.mapping_revision,sb.mapping_revision,sb.state,sb.identity_verified
		FROM gateways g JOIN devices d ON d.id=g.device_id JOIN sim_bindings sb ON sb.gateway_id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.role='gateway' AND d.state='active'
		  AND sb.sim_id=$3 AND sb.owner_id=$2 FOR UPDATE OF g,sb`, identity.deviceID, identity.ownerID, simID).
		Scan(&currentRevision, &simRevision, &simState, &simVerified)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (currentRevision != revision || simRevision != revision || simState != "active" || !simVerified) {
		return ErrSIMUnavailable
	}
	if err != nil {
		return err
	}
	if err := validateBindingTx(ctx, tx, identity.ownerID, client.deviceID, "client", client.endpointID); err != nil {
		return err
	}
	if err := m.preemptOrRejectSlot(ctx, tx, identity.deviceID); err != nil {
		return err
	}
	toAddress := channel.Connected.Number
	if toAddress == "" {
		toAddress = channel.Dialplan.Exten
	}
	fromAddress := channel.Caller.Number
	expiresAt := time.Now().UTC().Add(incomingCallLifetime)
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,
		 direction,from_address,to_address,state,state_revision,expires_at,wake_nonce,gateway_endpoint_id,
		 client_endpoint_id,gateway_channel_id,gateway_sip_call_id)
		VALUES($1,$2,$3,$4,$5,$6,'incoming',NULLIF($7,''),$8,'pending_wakeup',1,
		       clock_timestamp()+interval '25 seconds',$9,$10,$11,$12,$13)
		RETURNING expires_at`, callID, identity.ownerID, client.deviceID, identity.deviceID, simID, revision,
		fromAddress, toAddress, wakeNonce, endpointID, client.endpointID, channel.ID, "").Scan(&expiresAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, identity.deviceID, callID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, callID, identity.ownerID, client.deviceID, 1, "call.pending_wakeup", "pending_wakeup"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := m.ari.Progress(ctx, channel.ID); err != nil {
		_ = m.markUnknown(context.Background(), callID, "ari_progress_uncertain")
		return err
	}
	return nil
}

type primaryClient struct {
	deviceID   string
	endpointID string
}

func (m *Manager) loadPrimaryClient(ctx context.Context, ownerID string) (primaryClient, error) {
	var client primaryClient
	err := m.db.QueryRowContext(ctx, `
		SELECT d.id::text,b.endpoint_id FROM devices d JOIN sip_endpoint_bindings b ON b.device_id=d.id
		WHERE d.owner_id=$1 AND d.role='client' AND d.state='active' AND b.state='active'
		ORDER BY d.created_at,d.id LIMIT 1`, ownerID).Scan(&client.deviceID, &client.endpointID)
	if errors.Is(err, sql.ErrNoRows) {
		return primaryClient{}, ErrNotFound
	}
	return client, err
}

func (m *Manager) preemptOrRejectSlot(ctx context.Context, tx *sql.Tx, gatewayID string) error {
	var callID, state string
	var expires sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT c.call_id::text,c.state,c.expires_at FROM gateway_call_slots s JOIN call_sessions c ON c.call_id=s.call_id WHERE s.gateway_id=$1 FOR UPDATE OF s,c`, gatewayID).
		Scan(&callID, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "ended" {
		_, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID)
		return err
	}
	if state != "reserved" {
		return ErrBusy
	}
	var ownerID, clientID string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id::text,client_device_id::text,state_revision FROM call_sessions WHERE call_id=$1`, callID).
		Scan(&ownerID, &clientID, &revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state='cancelled',updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason='incoming_call_preempted',state_revision=state_revision+1,ended_at=clock_timestamp(),updated_at=clock_timestamp() WHERE call_id=$1 AND state='reserved'`, callID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_call_slots WHERE gateway_id=$1 AND call_id=$2`, gatewayID, callID); err != nil {
		return err
	}
	return appendEvent(ctx, tx, callID, ownerID, clientID, revision+1, "call.ended", "ended")
}

func (m *Manager) onCallLeg(ctx context.Context, endpointID string, channel asterisk.Channel, kind, callID string) error {
	identity, err := m.identityForEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	legs, err := loadLegsForUpdate(ctx, tx, callID)
	if err != nil {
		return err
	}
	if legs.ownerID != identity.ownerID {
		return ErrForbidden
	}
	if legs.state == "active" {
		if legs.gatewayChannel == channel.ID || legs.clientChannel == channel.ID {
			return tx.Commit()
		}
		return ErrConflict
	}
	var targetEndpoint, existingChannel string
	var field string
	switch kind {
	case "gateway-leg":
		if identity.role != "gateway" || identity.deviceID != legs.gatewayID || legs.direction != "outgoing" || legs.state != "dialing" && legs.state != "connecting" {
			return ErrConflict
		}
		targetEndpoint, existingChannel, field = legs.gatewayEndpoint, legs.gatewayChannel, "gateway_channel_id"
	case "client-leg":
		if identity.role != "client" || identity.deviceID != legs.clientID || legs.direction != "incoming" || legs.state != "ringing" && legs.state != "connecting" {
			return ErrConflict
		}
		targetEndpoint, existingChannel, field = legs.clientEndpoint, legs.clientChannel, "client_channel_id"
	default:
		return ErrInvalid
	}
	if targetEndpoint != endpointID {
		return ErrForbidden
	}
	if existingChannel != "" && existingChannel != channel.ID {
		return ErrConflict
	}
	var query string
	if field == "client_channel_id" {
		query = `UPDATE call_sessions SET client_channel_id=COALESCE(client_channel_id,$2),state='connecting',state_revision=state_revision+CASE WHEN state='connecting' THEN 0 ELSE 1 END,updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`
	} else {
		query = `UPDATE call_sessions SET gateway_channel_id=COALESCE(gateway_channel_id,$2),state='connecting',state_revision=state_revision+CASE WHEN state='connecting' THEN 0 ELSE 1 END,updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`
	}
	if _, err := tx.ExecContext(ctx, query, callID, channel.ID); err != nil {
		return err
	}
	if legs.state != "connecting" {
		if err := appendEvent(ctx, tx, callID, legs.ownerID, legs.clientID, legs.stateRevision+1, "call.connecting", "connecting"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return m.connectIfReady(ctx, callID)
}

func (m *Manager) connectIfReady(ctx context.Context, callID string) error {
	m.bridgeMu.Lock()
	defer m.bridgeMu.Unlock()
	legs, err := loadLegs(ctx, m.db, callID)
	if err != nil {
		return err
	}
	if legs.state == "ended" || legs.state == "active" || legs.clientChannel == "" || legs.gatewayChannel == "" {
		return nil
	}
	bridgeID := "gsm2sip-" + strings.ReplaceAll(callID, "-", "")
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	locked, err := loadLegsForUpdate(ctx, tx, callID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if locked.state == "ended" || locked.state == "active" || locked.clientChannel == "" || locked.gatewayChannel == "" {
		_ = tx.Rollback()
		return nil
	}
	if locked.bridgeID == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET bridge_id=$2,updated_at=clock_timestamp() WHERE call_id=$1 AND bridge_id IS NULL`, callID, bridgeID); err != nil {
			_ = tx.Rollback()
			return err
		}
	} else {
		bridgeID = locked.bridgeID
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	bridge, err := m.ari.GetBridge(ctx, bridgeID)
	if err != nil {
		var responseErr *asterisk.ResponseError
		if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
			return err
		}
		if err := m.ari.CreateBridge(ctx, bridgeID); err != nil {
			return err
		}
		bridge, err = m.ari.GetBridge(ctx, bridgeID)
		if err != nil {
			return err
		}
	}
	if !containsChannel(bridge.ChannelIDs, legs.clientChannel) {
		if err := m.ari.AddChannel(ctx, bridgeID, legs.clientChannel); err != nil {
			return err
		}
	}
	if !containsChannel(bridge.ChannelIDs, legs.gatewayChannel) {
		if err := m.ari.AddChannel(ctx, bridgeID, legs.gatewayChannel); err != nil {
			return err
		}
	}
	channels, err := m.ari.ListChannels(ctx)
	if err != nil {
		return err
	}
	states := make(map[string]string, len(channels))
	for _, channel := range channels {
		states[channel.ID] = channel.State
	}
	if legs.direction == "outgoing" {
		if !strings.EqualFold(states[legs.clientChannel], "up") {
			if err := m.ari.Answer(ctx, legs.clientChannel); err != nil {
				return err
			}
		}
	} else {
		if !strings.EqualFold(states[legs.gatewayChannel], "up") {
			if err := m.ari.Answer(ctx, legs.gatewayChannel); err != nil {
				return err
			}
		}
	}
	tx, err = beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := loadLegsForUpdate(ctx, tx, callID)
	if err != nil {
		return err
	}
	if current.state == "ended" || current.state == "active" {
		return tx.Commit()
	}
	if current.clientChannel != legs.clientChannel || current.gatewayChannel != legs.gatewayChannel {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET bridge_id=$2,state='active',state_revision=state_revision+1,answered_at=COALESCE(answered_at,clock_timestamp()),updated_at=clock_timestamp() WHERE call_id=$1 AND state<>'ended'`, callID, bridgeID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, callID, legs.ownerID, legs.clientID, current.stateRevision+1, "call.active", "active"); err != nil {
		return err
	}
	return tx.Commit()
}

func containsChannel(channels []string, target string) bool {
	for _, channel := range channels {
		if channel == target {
			return true
		}
	}
	return false
}

func loadLegs(ctx context.Context, q queryer, callID string) (callLegs, error) {
	return scanLegs(q.QueryRowContext(ctx, `
		SELECT call_id::text,owner_id::text,client_device_id::text,gateway_id::text,direction,state,state_revision,
	       client_endpoint_id,gateway_endpoint_id,COALESCE(client_channel_id,''),COALESCE(gateway_channel_id,''),COALESCE(ari_wrapper_channel_id,''),COALESCE(bridge_id,'')
		FROM call_sessions WHERE call_id=$1`, callID))
}

func loadLegsForUpdate(ctx context.Context, tx *sql.Tx, callID string) (callLegs, error) {
	return scanLegs(tx.QueryRowContext(ctx, `
		SELECT call_id::text,owner_id::text,client_device_id::text,gateway_id::text,direction,state,state_revision,
	       client_endpoint_id,gateway_endpoint_id,COALESCE(client_channel_id,''),COALESCE(gateway_channel_id,''),COALESCE(ari_wrapper_channel_id,''),COALESCE(bridge_id,'')
		FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID))
}

func scanLegs(row interface{ Scan(...any) error }) (callLegs, error) {
	var legs callLegs
	err := row.Scan(&legs.callID, &legs.ownerID, &legs.clientID, &legs.gatewayID, &legs.direction, &legs.state,
		&legs.stateRevision, &legs.clientEndpoint, &legs.gatewayEndpoint, &legs.clientChannel, &legs.gatewayChannel, &legs.wrapperChannel, &legs.bridgeID)
	return legs, err
}

func (m *Manager) onChannelDestroyed(ctx context.Context, channelID string, cause int, causeText string) error {
	if channelID == "" {
		return nil
	}
	var callID string
	err := m.db.QueryRowContext(ctx, `SELECT call_id::text FROM call_sessions WHERE client_channel_id=$1 OR gateway_channel_id=$1 OR ari_wrapper_channel_id=$1 LIMIT 1`, channelID).Scan(&callID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reason := "failed"
	if cause == 16 || strings.Contains(strings.ToLower(causeText), "normal") {
		reason = "remote_hangup"
	}
	legs, err := loadLegs(ctx, m.db, callID)
	if err != nil {
		return err
	}
	if err := m.markUnknown(ctx, callID, reason); err != nil {
		return err
	}
	channels, err := m.ari.ListChannels(ctx)
	if err != nil {
		return err
	}
	confirmedGone, err := m.stopCallChannels(ctx, callID, legs.clientChannel, legs.gatewayChannel, legs.wrapperChannel, legs.bridgeID, channels)
	if err != nil {
		return err
	}
	if confirmedGone {
		return m.finishCall(ctx, callID, reason)
	}
	return nil
}

func (m *Manager) finishCall(ctx context.Context, callID, reason string) error {
	tx, err := beginDurable(ctx, m.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerID, clientID, gatewayID, state string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT owner_id::text,client_device_id::text,gateway_id::text,state,state_revision FROM call_sessions WHERE call_id=$1 FOR UPDATE`, callID).
		Scan(&ownerID, &clientID, &gatewayID, &state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "ended" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_sessions SET state='ended',reason=$2,state_revision=state_revision+1,ended_at=clock_timestamp(),wake_nonce=NULL,updated_at=clock_timestamp() WHERE call_id=$1`, callID, reason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_intents SET state=CASE WHEN state='reserved' THEN 'expired' ELSE state END,updated_at=clock_timestamp() WHERE call_id=$1`, callID); err != nil {
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

func (m *Manager) reconcile(ctx context.Context) error {
	channels, err := m.ari.ListChannels(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(channels))
	for _, channel := range channels {
		live[channel.ID] = true
	}
	if err := m.expireDueCalls(ctx); err != nil {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.call_id::text,c.owner_id::text,c.client_device_id::text,c.gateway_id::text,c.state,
		       COALESCE(c.client_channel_id,''),COALESCE(c.gateway_channel_id,''),
	       COALESCE(c.ari_wrapper_channel_id,''),COALESCE(c.bridge_id,''),
	       EXISTS(
	         SELECT 1 FROM devices d
	         WHERE d.id=c.client_device_id AND d.owner_id=c.owner_id AND d.role='client' AND d.state='active'
	           AND EXISTS(SELECT 1 FROM sip_endpoint_bindings b WHERE b.device_id=d.id AND b.endpoint_id=c.client_endpoint_id AND b.state='active')
	           AND EXISTS(SELECT 1 FROM sessions s WHERE s.device_id=d.id AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp()))
	       ) AS client_valid,
	       EXISTS(
	         SELECT 1 FROM devices d
	         WHERE d.id=c.gateway_id AND d.owner_id=c.owner_id AND d.role='gateway' AND d.state='active'
	           AND EXISTS(SELECT 1 FROM sip_endpoint_bindings b WHERE b.device_id=d.id AND b.endpoint_id=c.gateway_endpoint_id AND b.state='active')
	           AND EXISTS(SELECT 1 FROM sessions s WHERE s.device_id=d.id AND (s.access_expires_at>clock_timestamp() OR s.refresh_expires_at>clock_timestamp()))
	       ) AS gateway_valid
		FROM call_sessions c WHERE c.state<>'ended'`)
	if err != nil {
		return err
	}
	var sessions []reconcileSession
	for rows.Next() {
		var item reconcileSession
		if err := rows.Scan(&item.callID, &item.ownerID, &item.clientID, &item.gatewayID, &item.state,
			&item.clientChannel, &item.gatewayChannel, &item.wrapperChannel, &item.bridgeID,
			&item.clientValid, &item.gatewayValid); err != nil {
			_ = rows.Close()
			return err
		}
		sessions = append(sessions, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, session := range sessions {
		if session.state == "reserved" {
			if !session.clientValid || !session.gatewayValid {
				if err := m.finishCall(ctx, session.callID, revokedCallReason(session)); err != nil {
					return err
				}
			}
			continue
		}
		if !session.clientValid || !session.gatewayValid || session.state == "unknown" {
			reason := revokedCallReason(session)
			if session.state == "unknown" && session.clientValid && session.gatewayValid {
				reason = "ari_reconciliation_uncertain"
			}
			if session.state != "unknown" {
				if err := m.markUnknown(ctx, session.callID, reason); err != nil {
					return err
				}
			}
			confirmedGone, err := m.stopCallChannels(ctx, session.callID, session.clientChannel, session.gatewayChannel, session.wrapperChannel, session.bridgeID, channels)
			if err != nil {
				return err
			}
			if confirmedGone {
				if err := m.finishCall(ctx, session.callID, reason); err != nil {
					return err
				}
			}
			continue
		}
		if session.state == "ended" {
			continue
		}
		missingClient := session.clientChannel != "" && !live[session.clientChannel]
		missingGateway := session.gatewayChannel != "" && !live[session.gatewayChannel]
		missingWrapper := session.wrapperChannel != "" && !live[session.wrapperChannel]
		uncertain := missingClient || missingGateway
		if session.state == "dialing" || session.state == "ringing" {
			uncertain = uncertain || session.wrapperChannel == "" || missingWrapper
		}
		if uncertain {
			if err := m.markUnknown(ctx, session.callID, "ari_reconciliation_uncertain"); err != nil {
				return err
			}
			confirmedGone, err := m.stopCallChannels(ctx, session.callID, session.clientChannel, session.gatewayChannel, session.wrapperChannel, session.bridgeID, channels)
			if err != nil {
				return err
			}
			if confirmedGone {
				if err := m.finishCall(ctx, session.callID, "ari_reconciliation_uncertain"); err != nil {
					return err
				}
			}
			continue
		}
		if session.state == "connecting" && session.clientChannel != "" && session.gatewayChannel != "" && live[session.clientChannel] && live[session.gatewayChannel] {
			if err := m.connectIfReady(ctx, session.callID); err != nil {
				return err
			}
		}
	}
	return nil
}

func revokedCallReason(session reconcileSession) string {
	if !session.clientValid {
		return "client_endpoint_revoked"
	}
	if !session.gatewayValid {
		return "gateway_endpoint_revoked"
	}
	return "call_interrupted"
}

// stopCallChannels returns confirmedGone only when a fresh ARI channel listing
// has no known or inherited call-linked channels. Failed hangup requests keep
// the call's unknown state and gateway lease for the next reconciliation pass.
func (m *Manager) stopCallChannels(ctx context.Context, callID, clientChannel, gatewayChannel, wrapperChannel, bridgeID string, snapshot []asterisk.Channel) (bool, error) {
	known := map[string]bool{}
	for _, id := range []string{clientChannel, gatewayChannel, wrapperChannel} {
		if id != "" {
			known[id] = true
		}
	}
	related, complete := relatedChannels(ctx, m.ari, snapshot, strings.ToLower(callID), known)
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
	stillRelated, scanComplete := relatedChannels(ctx, m.ari, remaining, strings.ToLower(callID), known)
	if len(stillRelated) > 0 || !complete || !scanComplete {
		return false, nil
	}
	return true, nil
}

func relatedChannels(ctx context.Context, ari *asterisk.Client, channels []asterisk.Channel, callID string, known map[string]bool) ([]asterisk.Channel, bool) {
	related := make([]asterisk.Channel, 0, len(known))
	complete := true
	for _, channel := range channels {
		if known[channel.ID] {
			related = append(related, channel)
			continue
		}
		value, err := ari.GetChannelVariable(ctx, channel.ID, "GSM2SIP_CALL_ID")
		if err != nil {
			var responseErr *asterisk.ResponseError
			if errors.As(err, &responseErr) && responseErr.StatusCode == 404 {
				continue
			}
			complete = false
			continue
		}
		if validUUID(value) && strings.EqualFold(value, callID) {
			related = append(related, channel)
		}
	}
	return related, complete
}

// failClosed marks every call whose progress depends on the lost ARI event
// stream as unknown and tears down any channels that can still be tied to it.
// Reserved intents have not crossed the one-shot client SIP boundary, so they
// remain eligible until their normal 30 second expiry.
func (m *Manager) failClosed(ctx context.Context, reason string) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT call_id::text,state,COALESCE(client_channel_id,''),COALESCE(gateway_channel_id,''),
		       COALESCE(ari_wrapper_channel_id,''),COALESCE(bridge_id,'')
		FROM call_sessions WHERE state NOT IN ('ended','reserved')`)
	if err != nil {
		return err
	}
	type interruptedCall struct {
		id, state, clientChannel, gatewayChannel, wrapperChannel, bridgeID string
	}
	calls := make([]interruptedCall, 0)
	for rows.Next() {
		var call interruptedCall
		if err := rows.Scan(&call.id, &call.state, &call.clientChannel, &call.gatewayChannel, &call.wrapperChannel, &call.bridgeID); err != nil {
			_ = rows.Close()
			return err
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(calls) == 0 {
		return nil
	}

	callByChannel := make(map[string]string, len(calls)*3)
	callIDs := make(map[string]bool, len(calls))
	bridgeIDs := make(map[string]bool)
	for _, call := range calls {
		callIDs[call.id] = true
		for _, channelID := range []string{call.clientChannel, call.gatewayChannel, call.wrapperChannel} {
			if channelID != "" {
				callByChannel[channelID] = call.id
			}
		}
		if call.bridgeID != "" {
			bridgeIDs[call.bridgeID] = true
		}
		if call.state != "unknown" {
			if err := m.markUnknown(ctx, call.id, reason); err != nil {
				return err
			}
		}
	}

	channels, err := m.ari.ListChannels(ctx)
	if err != nil {
		return err
	}
	for _, channel := range channels {
		callID := callByChannel[channel.ID]
		if callID == "" {
			// StasisStart may have been lost before the child channel ID was
			// committed. The inherited call ID is safe to use for cleanup only;
			// it never authenticates an endpoint or starts a call.
			value, variableErr := m.ari.GetChannelVariable(ctx, channel.ID, "GSM2SIP_CALL_ID")
			if variableErr != nil {
				var responseErr *asterisk.ResponseError
				if errors.As(variableErr, &responseErr) && responseErr.StatusCode == 404 {
					continue
				}
				return variableErr
			}
			if validUUID(value) && callIDs[strings.ToLower(value)] {
				callID = strings.ToLower(value)
			}
		}
		if callID == "" {
			continue
		}
		if err := m.ari.Hangup(ctx, channel.ID); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				return err
			}
		}
	}
	for bridgeID := range bridgeIDs {
		if err := m.ari.DestroyBridge(ctx, bridgeID); err != nil {
			var responseErr *asterisk.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 404 {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) expireDueCalls(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT call_id::text,owner_id::text,client_device_id::text,gateway_id::text,state,state_revision,
		       COALESCE(gateway_channel_id,''),COALESCE(client_channel_id,''),COALESCE(ari_wrapper_channel_id,'')
		FROM call_sessions WHERE state IN ('reserved','pending_wakeup','ringing') AND expires_at<=clock_timestamp()`)
	if err != nil {
		return err
	}
	type expired struct {
		callID, ownerID, clientID, gatewayID, state, gatewayChannel, clientChannel, wrapperChannel string
		revision                                                                                   int64
	}
	var items []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.callID, &item.ownerID, &item.clientID, &item.gatewayID, &item.state, &item.revision, &item.gatewayChannel, &item.clientChannel, &item.wrapperChannel); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, item := range items {
		tx, err := beginDurable(ctx, m.db)
		if err != nil {
			return err
		}
		var state string
		var revision int64
		err = tx.QueryRowContext(ctx, `SELECT state,state_revision FROM call_sessions WHERE call_id=$1 FOR UPDATE`, item.callID).Scan(&state, &revision)
		if err != nil {
			_ = tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if state != item.state || state == "ended" || state == "unknown" {
			_ = tx.Rollback()
			continue
		}
		reason := "intent_expired"
		if state == "pending_wakeup" || state == "ringing" {
			reason = "no_answer"
		}
		if err := m.endCallTx(ctx, tx, item.callID, item.ownerID, item.clientID, item.gatewayID, revision, reason); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if item.gatewayChannel != "" {
			_ = m.ari.Hangup(ctx, item.gatewayChannel)
		}
		if item.clientChannel != "" {
			_ = m.ari.Hangup(ctx, item.clientChannel)
		}
		if item.wrapperChannel != "" {
			_ = m.ari.Hangup(ctx, item.wrapperChannel)
		}
	}
	return nil
}

func (m *Manager) closeOrphan(ctx context.Context, channelID string) error {
	if channelID == "" {
		return nil
	}
	return m.ari.Hangup(ctx, channelID)
}

func stateError(state string) error { return fmt.Errorf("invalid call state %q", state) }
