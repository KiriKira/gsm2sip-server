package httpapi

import (
	"net/http"
	"time"
)

func (s *Server) acknowledgeClientEvents(w http.ResponseWriter, r *http.Request, p Principal) {
	if p.Role != "client" {
		writeError(w, 403, "CLIENT_REQUIRED", "Only a host client can report durable reception.", false)
		return
	}
	var request struct {
		Cursor string `json:"durable_cursor"`
	}
	if !decodeOrError(w, r, &request, 4096) {
		return
	}
	cursor, err := parseEventCursor(request.Cursor)
	if err != nil || request.Cursor == "" {
		writeError(w, 400, "INVALID_CURSOR", "A persisted event cursor is required.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	var maximum int64
	if err = tx.QueryRowContext(r.Context(), `SELECT COALESCE((SELECT last_cursor FROM owner_event_cursors WHERE owner_id=$1),0)`, p.OwnerID).Scan(&maximum); err != nil {
		writeDBUnavailable(w)
		return
	}
	if cursor > maximum {
		writeError(w, 409, "CURSOR_AHEAD", "Receipt cannot exceed this owner's committed event feed.", false)
		return
	}
	var retained int64
	var confirmed time.Time
	err = tx.QueryRowContext(r.Context(), `INSERT INTO client_event_receipts(device_id,owner_id,durable_cursor) VALUES($1,$2,$3) ON CONFLICT(device_id) DO UPDATE SET durable_cursor=GREATEST(client_event_receipts.durable_cursor,EXCLUDED.durable_cursor),confirmed_at=CASE WHEN EXCLUDED.durable_cursor>=client_event_receipts.durable_cursor THEN now() ELSE client_event_receipts.confirmed_at END RETURNING durable_cursor,confirmed_at`, p.DeviceID, p.OwnerID, cursor).Scan(&retained, &confirmed)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if err = tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, 200, map[string]any{"durable_cursor": opaqueEventCursor(retained), "confirmed_at": confirmed, "events_retained": true})
}
