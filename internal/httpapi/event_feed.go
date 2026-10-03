package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a client can read the owner event feed.", false)
		return
	}
	cursor, err := parseEventCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CURSOR", "Event cursor is invalid.", false)
		return
	}
	limit := queryLimit(r)
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(r.Context(), `
		SELECT cursor,event_json FROM server_events WHERE owner_id=$1 AND cursor>$2
		ORDER BY cursor LIMIT $3`, principal.OwnerID, cursor, limit+1)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	type row struct {
		cursor int64
		body   []byte
	}
	items := make([]json.RawMessage, 0, limit+1)
	var rowsData []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.cursor, &item.body); err != nil {
			_ = rows.Close()
			writeDBUnavailable(w)
			return
		}
		rowsData = append(rowsData, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	hasMore := len(rowsData) > limit
	if hasMore {
		rowsData = rowsData[:limit]
	}
	messageDetails := make(map[string]MessageDetail)
	for _, item := range rowsData {
		var decoded map[string]any
		if err := json.Unmarshal(item.body, &decoded); err != nil {
			writeError(w, http.StatusInternalServerError, "EVENT_CORRUPT", "Stored event cannot be read.", true)
			return
		}
		var eventSnapshot struct {
			Message struct {
				MessageID string `json:"message_id"`
			} `json:"message"`
		}
		if err := json.Unmarshal(item.body, &eventSnapshot); err != nil || !validUUID(eventSnapshot.Message.MessageID) {
			writeError(w, http.StatusInternalServerError, "EVENT_CORRUPT", "Stored event does not identify its message.", true)
			return
		}
		detail, ok := messageDetails[eventSnapshot.Message.MessageID]
		if !ok {
			var err error
			detail, err = loadMessageDetail(r.Context(), tx, eventSnapshot.Message.MessageID)
			if err != nil {
				writeDBUnavailable(w)
				return
			}
			messageDetails[eventSnapshot.Message.MessageID] = detail
		}
		decoded["message"] = detail
		decoded["cursor"] = opaqueEventCursor(item.cursor)
		body, err := json.Marshal(decoded)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EVENT_CORRUPT", "Stored event cannot be read.", true)
			return
		}
		items = append(items, body)
	}
	var nextCursor *string
	if hasMore && len(rowsData) > 0 {
		value := opaqueEventCursor(rowsData[len(rowsData)-1].cursor)
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor, "resync_required": false})
}
