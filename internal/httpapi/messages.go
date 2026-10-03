package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) createMessage(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only an authorized client can create an SMS task.", false)
		return
	}
	var request CreateMessageRequest
	if !decodeOrError(w, r, &request, 24*1024) {
		return
	}
	request.GatewayID = strings.ToLower(request.GatewayID)
	request.SIMID = strings.ToLower(request.SIMID)
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 300
	}
	if !validUUID(request.GatewayID) || !validUUID(request.SIMID) || request.MappingRevision < 1 ||
		!validAddress(request.To) || !validText(request.Text) || request.TTLSeconds < 1 || request.TTLSeconds > 3600 {
		writeError(w, http.StatusBadRequest, "INVALID_MESSAGE", "Message recipient, text, SIM, revision, or TTL is invalid.", false)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || strings.TrimSpace(idempotencyKey) != idempotencyKey || strings.ContainsAny(idempotencyKey, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 visible characters.", false)
		return
	}
	payloadHash, err := hashJSON(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MESSAGE", "Message payload is invalid.", false)
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
	lockName := strings.Join([]string{principal.OwnerID, principal.DeviceID, "messages.create", idempotencyKey}, ":")
	if _, err := tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockName); err != nil {
		writeDBUnavailable(w)
		return
	}
	var previousHash, previousResponse []byte
	var previousStatus int
	err = tx.QueryRowContext(r.Context(), `
		SELECT payload_hash, response_status, response_body
		FROM idempotency_records WHERE owner_id=$1 AND device_id=$2 AND operation='messages.create' AND idempotency_key=$3`,
		principal.OwnerID, principal.DeviceID, idempotencyKey).Scan(&previousHash, &previousStatus, &previousResponse)
	if err == nil {
		if !bytesEqual(payloadHash, previousHash) {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency-Key was reused with a different message payload.", false)
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBUnavailable(w)
			return
		}
		writeRawJSON(w, previousStatus, previousResponse)
		return
	}
	if err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	var currentRevision, simRevision int64
	var bindingState string
	var identityVerified bool
	err = tx.QueryRowContext(r.Context(), `
		SELECT g.mapping_revision, b.mapping_revision, b.state, b.identity_verified
		FROM gateways g JOIN devices d ON d.id=g.device_id
		JOIN sim_bindings b ON b.gateway_id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.role='gateway' AND d.state='active'
		AND b.sim_id=$3 AND b.owner_id=$2 FOR SHARE OF g,b`, request.GatewayID, principal.OwnerID, request.SIMID).
		Scan(&currentRevision, &simRevision, &bindingState, &identityVerified)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if bindingState != "active" || !identityVerified {
		writeError(w, http.StatusConflict, "SIM_UNAVAILABLE", "SIM is not locally confirmed and active.", false)
		return
	}
	if request.MappingRevision != currentRevision || simRevision != currentRevision {
		writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "SIM mapping revision is stale.", false)
		return
	}
	messageID, commandID := newUUID(), newUUID()
	if limited, err := consumeSMSBudget(r.Context(), tx, request.SIMID, messageID); err != nil {
		writeDBUnavailable(w)
		return
	} else if limited {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "SIM send budget is 5 messages per rolling minute and 30 per rolling hour.", false)
		return
	}
	createdAt := s.now().UTC()
	expiresAt := createdAt.Add(time.Duration(request.TTLSeconds) * time.Second)
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO messages(id, owner_id, client_device_id, gateway_id, sim_id,
		mapping_revision, direction, to_address, body, status, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'outbound',$7,$8,'queued',$9,$10,$10)`,
		messageID, principal.OwnerID, principal.DeviceID, request.GatewayID, request.SIMID,
		request.MappingRevision, request.To, request.Text, expiresAt, createdAt); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO commands(id, message_id, owner_id, gateway_id, sim_id,
		mapping_revision, to_address, body, status, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued',$9,$10,$10)`,
		commandID, messageID, principal.OwnerID, request.GatewayID, request.SIMID,
		request.MappingRevision, request.To, request.Text, expiresAt, createdAt); err != nil {
		writeDBUnavailable(w)
		return
	}
	response := MessageAccepted{MessageID: messageID, CommandID: commandID, Status: "queued", CreatedAt: createdAt, ExpiresAt: expiresAt}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO idempotency_records(owner_id,device_id,operation,idempotency_key,
		payload_hash,response_status,response_body,resource_id)
		VALUES ($1,$2,'messages.create',$3,$4,$5,$6,$7)`,
		principal.OwnerID, principal.DeviceID, idempotencyKey, payloadHash, http.StatusAccepted, string(responseJSON), messageID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := s.addClientEvent(r.Context(), tx, principal.OwnerID, messageID, "message.created"); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO audit_log(owner_id, actor_device_id, action, resource_id)
		VALUES ($1,$2,'sms.command_created',$3)`, principal.OwnerID, principal.DeviceID, messageID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusAccepted, response)
}

func consumeSMSBudget(ctx context.Context, tx *sql.Tx, simID, messageID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, simID); err != nil {
		return false, err
	}
	var minuteCount, hourCount int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE created_at > clock_timestamp() - interval '1 minute'),
		COUNT(*) FILTER (WHERE created_at > clock_timestamp() - interval '1 hour')
		FROM sms_rate_ledger WHERE sim_id=$1`, simID).Scan(&minuteCount, &hourCount)
	if err != nil {
		return false, err
	}
	if minuteCount >= 5 || hourCount >= 30 {
		return true, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sms_rate_ledger(sim_id,message_id) VALUES ($1,$2)`, simID, messageID)
	return false, err
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a client can read remote messages.", false)
		return
	}
	messageID := strings.ToLower(r.PathValue("message_id"))
	if !validUUID(messageID) {
		writeNotFound(w)
		return
	}
	var exists bool
	if err := s.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1 AND owner_id=$2)`, messageID, principal.OwnerID).Scan(&exists); err != nil {
		writeDBUnavailable(w)
		return
	}
	if !exists {
		writeNotFound(w)
		return
	}
	detail, err := loadMessageDetail(r.Context(), s.db, messageID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a client can list remote messages.", false)
		return
	}
	limit := queryLimit(r)
	createdCursor, idCursor, err := parseMessageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CURSOR", "Message cursor is invalid.", false)
		return
	}
	simID := strings.ToLower(r.URL.Query().Get("sim_id"))
	if simID != "" {
		if !validUUID(simID) {
			writeNotFound(w)
			return
		}
		var exists bool
		if err := s.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM sim_bindings WHERE sim_id=$1 AND owner_id=$2)`, simID, principal.OwnerID).Scan(&exists); err != nil {
			writeDBUnavailable(w)
			return
		}
		if !exists {
			writeNotFound(w)
			return
		}
	}
	query := `SELECT id::text, created_at FROM messages WHERE owner_id=$1`
	args := []any{principal.OwnerID}
	if simID != "" {
		args = append(args, simID)
		query += fmt.Sprintf(` AND sim_id=$%d`, len(args))
	}
	if !createdCursor.IsZero() {
		args = append(args, createdCursor, idCursor)
		query += fmt.Sprintf(` AND (created_at,id) < ($%d,$%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT $%d`, len(args))
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	type entry struct {
		id      string
		created time.Time
	}
	entries := make([]entry, 0, limit+1)
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.id, &item.created); err != nil {
			_ = rows.Close()
			writeDBUnavailable(w)
			return
		}
		entries = append(entries, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	items := make([]MessageDetail, 0, len(entries))
	for _, item := range entries {
		detail, err := loadMessageDetail(r.Context(), s.db, item.id)
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		items = append(items, detail)
	}
	var nextCursor *string
	if hasMore && len(entries) > 0 {
		value := messageCursor(entries[len(entries)-1].created, entries[len(entries)-1].id)
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func queryLimit(r *http.Request) int {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 50
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
}
