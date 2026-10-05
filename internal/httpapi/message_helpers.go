package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func lockOwnerEventCursor(ctx context.Context, tx *sql.Tx, ownerID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO owner_event_cursors(owner_id,last_cursor) VALUES ($1,0)
		ON CONFLICT (owner_id) DO NOTHING`, ownerID); err != nil {
		return err
	}
	var cursor int64
	return tx.QueryRowContext(ctx, `SELECT last_cursor FROM owner_event_cursors WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&cursor)
}

func loadMessageDetail(ctx context.Context, q queryer, messageID string) (MessageDetail, error) {
	var detail MessageDetail
	var commandID, simID, from, to sql.NullString
	var mappingRevision, partCount sql.NullInt64
	var expiresAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT m.id::text, c.id::text, m.gateway_id::text, m.sim_id::text,
		m.mapping_revision, m.direction, m.from_address, m.to_address, m.body, m.status,
		m.part_count, m.created_at, m.expires_at
		FROM messages m LEFT JOIN commands c ON c.message_id=m.id WHERE m.id=$1`, messageID).Scan(
		&detail.MessageID, &commandID, &detail.GatewayID, &simID,
		&mappingRevision, &detail.Direction, &from, &to, &detail.Text,
		&detail.Status, &partCount, &detail.CreatedAt, &expiresAt)
	if err != nil {
		return MessageDetail{}, err
	}
	detail.CommandID = nullableString(commandID)
	detail.SIMID = nullableString(simID)
	detail.MappingRevision = nullableInt64(mappingRevision)
	detail.From = nullableString(from)
	detail.To = nullableString(to)
	detail.PartCount = nullableInt(partCount)
	if expiresAt.Valid {
		value := expiresAt.Time
		detail.ExpiresAt = &value
	}
	detail.Parts = make([]MessagePart, 0)
	rows, err := q.QueryContext(ctx, `
		SELECT part_index, state, result_code, error
		FROM message_parts WHERE message_id=$1 ORDER BY part_index`, messageID)
	if err != nil {
		return MessageDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var part MessagePart
		var resultCode sql.NullInt64
		var message sql.NullString
		if err := rows.Scan(&part.PartIndex, &part.State, &resultCode, &message); err != nil {
			return MessageDetail{}, err
		}
		if resultCode.Valid {
			value := int(resultCode.Int64)
			part.ResultCode = &value
		}
		part.Error = nullableString(message)
		detail.Parts = append(detail.Parts, part)
	}
	return detail, rows.Err()
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	integer := value.Int64
	return &integer
}

func nullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	integer := int(value.Int64)
	return &integer
}

func (s *Server) addClientEvent(ctx context.Context, tx *sql.Tx, ownerID, messageID, eventType string) error {
	var cursor int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE owner_event_cursors SET last_cursor=last_cursor+1
		WHERE owner_id=$1 RETURNING last_cursor`, ownerID).Scan(&cursor); err != nil {
		return err
	}
	detail, err := loadMessageDetail(ctx, tx, messageID)
	if err != nil {
		return err
	}
	id := newUUID()
	createdAt := s.now().UTC()
	body, err := json.Marshal(map[string]any{
		"event_id":    id,
		"type":        eventType,
		"occurred_at": createdAt,
		"message":     detail,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO server_events(cursor,id,owner_id,event_type,message_id,event_json,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, cursor, id, ownerID, eventType, messageID, string(body), createdAt); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox(owner_id, event_id, kind, payload) VALUES ($1,$2,'client_event',$3)`, ownerID, id, string(body))
	return err
}

func (s *Server) setCommandAndMessageStatus(ctx context.Context, tx *sql.Tx, commandID, messageID, ownerID, status, eventType string) error {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&current); err != nil {
		return err
	}
	if current == "delivered" || current == "failed" || current == "expired" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET status=$1, updated_at=now() WHERE id=$2`, status, commandID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status=$1, updated_at=now() WHERE id=$2`, status, messageID); err != nil {
		return err
	}
	if status != current {
		return s.addClientEvent(ctx, tx, ownerID, messageID, eventType)
	}
	return nil
}

func (s *Server) aggregateParts(ctx context.Context, tx *sql.Tx, commandID, messageID, ownerID string, partChanged bool) error {
	var count sql.NullInt64
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT part_count, status FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&count, &current); err != nil {
		return err
	}
	if !count.Valid || count.Int64 < 1 {
		return nil
	}
	var total, delivered, submitted, failed, expired, unknown, dispatched int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		COUNT(*) FILTER (WHERE state='delivered'),
		COUNT(*) FILTER (WHERE state IN ('submitted','delivered')),
		COUNT(*) FILTER (WHERE state='failed'),
		COUNT(*) FILTER (WHERE state='expired'),
		COUNT(*) FILTER (WHERE state='unknown'),
		COUNT(*) FILTER (WHERE state='dispatching')
		FROM message_parts WHERE message_id=$1`, messageID).Scan(
		&total, &delivered, &submitted, &failed, &expired, &unknown, &dispatched); err != nil {
		return err
	}
	if total != int(count.Int64) {
		return fmt.Errorf("message %s has %d parts, expected %d", messageID, total, count.Int64)
	}
	status := "dispatching"
	switch {
	case delivered == total:
		status = "delivered"
	case dispatched > 0:
		status = "dispatching"
	case unknown > 0:
		status = "unknown"
	case failed > 0:
		status = "failed"
	case expired == total:
		status = "expired"
	case expired > 0:
		status = "failed"
	case submitted == total:
		status = "submitted"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET status=$1, updated_at=now() WHERE id=$2`, status, commandID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status=$1, updated_at=now() WHERE id=$2`, status, messageID); err != nil {
		return err
	}
	if status != current || partChanged {
		return s.addClientEvent(ctx, tx, ownerID, messageID, "message.state_changed")
	}
	return nil
}

func commandExpiryState(expiration time.Time, now time.Time) bool {
	return !expiration.After(now)
}
