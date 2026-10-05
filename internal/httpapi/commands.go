package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type commandClaimRequest struct {
	PayloadSHA256 string `json:"payload_sha256"`
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	if principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "GATEWAY_REQUIRED", "Only the paired gateway can retrieve its commands.", false)
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	if !s.expireQueuedForGateway(r, gatewayID, principal.OwnerID) {
		writeDBUnavailable(w)
		return
	}
	limit := commandPageLimit(r)
	createdCursor, idCursor, err := parseMessageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CURSOR", "Command cursor is invalid.", false)
		return
	}
	query := `
		SELECT c.id::text, c.message_id::text, c.gateway_id::text, c.sim_id::text,
		c.mapping_revision, c.to_address, c.body, c.expires_at, c.status, c.created_at
		FROM commands c WHERE c.gateway_id=$1 AND c.owner_id=$2
		AND c.status IN ('queued','accepted_by_gateway','dispatching')
		AND (c.status <> 'queued' OR c.expires_at > now())`
	args := []any{gatewayID, principal.OwnerID}
	if !createdCursor.IsZero() {
		args = append(args, createdCursor, idCursor)
		query += fmt.Sprintf(` AND (c.created_at,c.id) > ($%d,$%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(` ORDER BY c.created_at,c.id LIMIT $%d`, len(args))
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	commands := make([]Command, 0, limit+1)
	for rows.Next() {
		var command Command
		if err := rows.Scan(&command.CommandID, &command.MessageID, &command.GatewayID,
			&command.SIMID, &command.MappingRevision, &command.To, &command.Text,
			&command.ExpiresAt, &command.State, &command.CreatedAt); err != nil {
			_ = rows.Close()
			writeDBUnavailable(w)
			return
		}
		command.PayloadSHA256 = commandDigest(command)
		commands = append(commands, command)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	hasMore := len(commands) > limit
	if hasMore {
		commands = commands[:limit]
	}
	var nextCursor *string
	if hasMore && len(commands) > 0 {
		last := commands[len(commands)-1]
		value := messageCursor(last.CreatedAt, last.CommandID)
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": commands, "next_cursor": nextCursor})
}

func commandPageLimit(r *http.Request) int {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 100
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 100
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func (s *Server) claimCommand(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	if principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "GATEWAY_REQUIRED", "Only the paired gateway can claim commands.", false)
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	commandID := strings.ToLower(r.PathValue("command_id"))
	if !validUUID(commandID) {
		writeNotFound(w)
		return
	}
	var request commandClaimRequest
	if !decodeOrError(w, r, &request, 4096) {
		return
	}
	if len(request.PayloadSHA256) != 64 {
		writeError(w, http.StatusBadRequest, "INVALID_COMMAND_HASH", "payload_sha256 must be a lowercase SHA-256 digest.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	if err := lockOwnerEventCursor(r.Context(), tx, principal.OwnerID); err != nil {
		writeDBUnavailable(w)
		return
	}
	var command Command
	err = tx.QueryRowContext(r.Context(), `
		SELECT c.id::text,c.message_id::text,c.gateway_id::text,c.sim_id::text,
		c.mapping_revision,c.to_address,c.body,c.expires_at,c.status
		FROM commands c JOIN devices d ON d.id=c.gateway_id
		WHERE c.id=$1 AND c.gateway_id=$2 AND c.owner_id=$3 AND d.state='active'
		FOR UPDATE OF c`, commandID, gatewayID, principal.OwnerID).
		Scan(&command.CommandID, &command.MessageID, &command.GatewayID, &command.SIMID,
			&command.MappingRevision, &command.To, &command.Text, &command.ExpiresAt, &command.State)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	command.PayloadSHA256 = commandDigest(command)
	if request.PayloadSHA256 != command.PayloadSHA256 {
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Command payload hash does not match the server-issued task.", false)
		return
	}
	if command.State == "queued" && commandExpiryState(command.ExpiresAt, s.now().UTC()) {
		if err := s.finishBeforeDispatch(r, tx, principal.OwnerID, command, "expired", "Command expired before claim."); err != nil {
			writeDBUnavailable(w)
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBUnavailable(w)
			return
		}
		writeError(w, http.StatusConflict, "COMMAND_EXPIRED", "Command expired before gateway claim.", false)
		return
	}
	if command.State == "queued" {
		var gatewayRevision, simRevision int64
		var simState string
		var identityVerified bool
		err := tx.QueryRowContext(r.Context(), `
			SELECT g.mapping_revision,b.mapping_revision,b.state,b.identity_verified
			FROM gateways g JOIN sim_bindings b ON b.gateway_id=g.device_id
			WHERE g.device_id=$1 AND b.sim_id=$2 FOR SHARE OF g,b`, gatewayID, command.SIMID).
			Scan(&gatewayRevision, &simRevision, &simState, &identityVerified)
		if err == sql.ErrNoRows || err == nil && (gatewayRevision != command.MappingRevision || simRevision != command.MappingRevision || simState != "active" || !identityVerified) {
			if err := s.finishBeforeDispatch(r, tx, principal.OwnerID, command, "failed", "SIM mapping is unavailable or changed."); err != nil {
				writeDBUnavailable(w)
				return
			}
			if err := tx.Commit(); err != nil {
				writeDBUnavailable(w)
				return
			}
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "Command SIM mapping is no longer active at the requested revision.", false)
			return
		}
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE commands SET status='accepted_by_gateway', claimed_at=now(), updated_at=now()
			WHERE id=$1 AND status='queued'`, command.CommandID); err != nil {
			writeDBUnavailable(w)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET status='accepted_by_gateway', updated_at=now() WHERE id=$1`, command.MessageID); err != nil {
			writeDBUnavailable(w)
			return
		}
		command.State = "accepted_by_gateway"
		if err := s.addClientEvent(r.Context(), tx, principal.OwnerID, command.MessageID, "message.state_changed"); err != nil {
			writeDBUnavailable(w)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO audit_log(owner_id,actor_device_id,action,resource_id)
			VALUES ($1,$2,'sms.command_claimed',$3)`, principal.OwnerID, principal.DeviceID, command.MessageID); err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	if command.State != "accepted_by_gateway" && command.State != "dispatching" {
		writeError(w, http.StatusConflict, "COMMAND_NOT_CLAIMABLE", "Command is already terminal and cannot be claimed again.", false)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, command)
}

func (s *Server) finishBeforeDispatch(r *http.Request, tx *sql.Tx, ownerID string, command Command, state, reason string) error {
	if _, err := tx.ExecContext(r.Context(), `UPDATE commands SET status=$1,updated_at=now() WHERE id=$2`, state, command.CommandID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET status=$1,updated_at=now() WHERE id=$2`, state, command.MessageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO audit_log(owner_id,action,resource_id,details)
		VALUES ($1,'sms.command_terminal_before_dispatch',$2,jsonb_build_object('state',$3,'reason',$4))`,
		ownerID, command.MessageID, state, reason); err != nil {
		return err
	}
	return s.addClientEvent(r.Context(), tx, ownerID, command.MessageID, "message.state_changed")
}

func (s *Server) expireQueuedForGateway(r *http.Request, gatewayID, ownerID string) bool {
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if err := lockOwnerEventCursor(r.Context(), tx, ownerID); err != nil {
		return false
	}
	rows, err := tx.QueryContext(r.Context(), `
		SELECT id::text,message_id::text FROM commands
		WHERE gateway_id=$1 AND owner_id=$2 AND status='queued' AND expires_at<=now()
		FOR UPDATE`, gatewayID, ownerID)
	if err != nil {
		return false
	}
	type expiredItem struct{ command, message string }
	var items []expiredItem
	for rows.Next() {
		var item expiredItem
		if err := rows.Scan(&item.command, &item.message); err != nil {
			_ = rows.Close()
			return false
		}
		items = append(items, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false
	}
	for _, item := range items {
		if _, err := tx.ExecContext(r.Context(), `UPDATE commands SET status='expired',updated_at=now() WHERE id=$1`, item.command); err != nil {
			return false
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET status='expired',updated_at=now() WHERE id=$1`, item.message); err != nil {
			return false
		}
		if err := s.addClientEvent(r.Context(), tx, ownerID, item.message, "message.state_changed"); err != nil {
			return false
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO audit_log(owner_id,action,resource_id,details)
			VALUES ($1,'sms.command_expired',$2,'{}'::jsonb)`, ownerID, item.message); err != nil {
			return false
		}
	}
	return tx.Commit() == nil
}

// ExpireQueued permanently expires only commands that were never claimed. A
// claim always stays attached to its gateway until a gateway event resolves it.
func (s *Server) ExpireQueued(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT owner_id::text FROM commands
		WHERE status='queued' AND expires_at<=now() ORDER BY owner_id LIMIT 100`)
	if err != nil {
		return 0, err
	}
	owners := make([]string, 0, 100)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		owners = append(owners, ownerID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	expiredCount := 0
	for _, ownerID := range owners {
		tx, err := beginDurable(ctx, s.db)
		if err != nil {
			return expiredCount, err
		}
		if err := lockOwnerEventCursor(ctx, tx, ownerID); err != nil {
			_ = tx.Rollback()
			return expiredCount, err
		}
		ownerRows, err := tx.QueryContext(ctx, `
			SELECT id::text,message_id::text FROM commands
			WHERE owner_id=$1 AND status='queued' AND expires_at<=now()
			ORDER BY expires_at,id LIMIT 100 FOR UPDATE SKIP LOCKED`, ownerID)
		if err != nil {
			_ = tx.Rollback()
			return expiredCount, err
		}
		type expiredItem struct{ commandID, messageID string }
		items := make([]expiredItem, 0)
		for ownerRows.Next() {
			var item expiredItem
			if err := ownerRows.Scan(&item.commandID, &item.messageID); err != nil {
				_ = ownerRows.Close()
				_ = tx.Rollback()
				return expiredCount, err
			}
			items = append(items, item)
		}
		err = ownerRows.Err()
		_ = ownerRows.Close()
		if err != nil {
			_ = tx.Rollback()
			return expiredCount, err
		}
		for _, item := range items {
			if _, err := tx.ExecContext(ctx, `UPDATE commands SET status='expired',updated_at=now() WHERE id=$1 AND status='queued'`, item.commandID); err != nil {
				_ = tx.Rollback()
				return expiredCount, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET status='expired',updated_at=now() WHERE id=$1 AND status='queued'`, item.messageID); err != nil {
				_ = tx.Rollback()
				return expiredCount, err
			}
			if err := s.addClientEvent(ctx, tx, ownerID, item.messageID, "message.state_changed"); err != nil {
				_ = tx.Rollback()
				return expiredCount, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO audit_log(owner_id,action,resource_id,details)
				VALUES ($1,'sms.command_expired',$2,'{}'::jsonb)`, ownerID, item.messageID); err != nil {
				_ = tx.Rollback()
				return expiredCount, err
			}
		}
		if err := tx.Commit(); err != nil {
			return expiredCount, err
		}
		expiredCount += len(items)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sms_rate_ledger WHERE created_at < clock_timestamp() - interval '2 hours'`); err != nil {
		return 0, err
	}
	return expiredCount, nil
}
