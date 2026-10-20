package calls

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// History reads the shared call records. Calls are created and advanced by the
// RTC call controller; rows written by older SIP releases stay readable.
type History struct {
	db *sql.DB
}

func NewHistory(database *sql.DB) *History {
	return &History{db: database}
}

// Each client sees its own participant state of an incoming fork, and the call
// state of the calls it placed.
const callColumns = `
		SELECT c.call_id::text,c.gateway_id::text,
		       COALESCE(p.client_device_id,c.client_device_id)::text,c.sim_id::text,c.mapping_revision,c.direction,
		       CASE WHEN p.client_device_id IS NOT NULL AND c.state='unknown' AND p.state<>'ended' THEN 'unknown'
		            WHEN p.client_device_id IS NULL THEN c.state
		            WHEN p.state='candidate' THEN 'pending_wakeup'
		            WHEN p.state='accepted' AND c.state='active' THEN 'active'
		            WHEN p.state='accepted' THEN 'connecting'
		            ELSE p.state END,
		       COALESCE(p.state_revision,c.state_revision),c.from_address,c.to_address,
		       CASE WHEN p.client_device_id IS NULL OR c.state='unknown' AND p.state<>'ended' THEN c.reason ELSE p.reason END,
		       c.created_at,c.expires_at,c.answered_at
		FROM call_sessions c LEFT JOIN call_participants p
		  ON p.call_id=c.call_id AND p.owner_id=c.owner_id AND p.client_device_id=$2`

func (h *History) ListCalls(ctx context.Context, ownerID, clientID, cursor string, limit int) ([]Call, string, error) {
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
	query := callColumns + `
		WHERE c.owner_id=$1 AND (c.client_device_id=$2 OR p.client_device_id=$2)`
	args := []any{ownerID, clientID}
	if !createdAt.IsZero() {
		query += ` AND (c.created_at,c.call_id)<($3,$4)`
		args = append(args, createdAt, callID)
	}
	args = append(args, limit+1)
	query += ` ORDER BY c.created_at DESC,c.call_id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := h.db.QueryContext(ctx, query, args...)
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

func (h *History) GetCall(ctx context.Context, ownerID, clientID, callID string) (Call, error) {
	if !validUUID(ownerID) || !validUUID(clientID) || !validUUID(callID) {
		return Call{}, ErrInvalid
	}
	item, err := scanCall(h.db.QueryRowContext(ctx, callColumns+`
		WHERE c.call_id=$3 AND c.owner_id=$1 AND (c.client_device_id=$2 OR p.client_device_id=$2)`, ownerID, clientID, callID))
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Call{}, err
	}
	var inOwner bool
	if err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM call_sessions WHERE call_id=$1 AND owner_id=$2)`, callID, ownerID).Scan(&inOwner); err != nil {
		return Call{}, err
	}
	if inOwner {
		return Call{}, ErrForbidden
	}
	return Call{}, ErrNotFound
}

type rowScanner interface{ Scan(...any) error }

func scanCall(row rowScanner) (Call, error) {
	var result Call
	var from, reason sql.NullString
	var expires, answered sql.NullTime
	err := row.Scan(&result.CallID, &result.GatewayID, &result.ClientID, &result.SIMID, &result.MappingRevision,
		&result.Direction, &result.State, &result.StateRevision, &from, &result.To, &reason, &result.CreatedAt, &expires, &answered)
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
	if answered.Valid {
		result.AnsweredAt = &answered.Time
	}
	return result, nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(strings.ToLower(value), "-", "")
	_, err := hex.DecodeString(compact)
	return err == nil
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
