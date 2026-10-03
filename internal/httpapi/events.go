package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

type eventBatchRequest struct {
	Events []GatewayEvent `json:"events"`
}

func (s *Server) uploadEvents(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	if principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "GATEWAY_REQUIRED", "Only the paired gateway can upload its events.", false)
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	var request eventBatchRequest
	if !decodeOrError(w, r, &request, maxJSONBody) {
		return
	}
	if len(request.Events) < 1 || len(request.Events) > 50 {
		writeError(w, http.StatusBadRequest, "INVALID_EVENT_BATCH", "A batch must contain between 1 and 50 events.", false)
		return
	}
	for i := range request.Events {
		event := &request.Events[i]
		event.EventID = strings.ToLower(event.EventID)
		event.GatewayID = strings.ToLower(event.GatewayID)
		if event.SIMID != nil {
			*event.SIMID = strings.ToLower(*event.SIMID)
		}
		if event.ProtocolVersion != 1 || event.GatewayID != gatewayID || !validUUID(event.EventID) || event.Sequence < 1 || event.OccurredAt.IsZero() {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Event envelope is invalid or does not belong to this gateway.", false)
			return
		}
		if i > 0 && event.Sequence <= request.Events[i-1].Sequence {
			writeError(w, http.StatusBadRequest, "INVALID_EVENT_SEQUENCE", "Events in a batch must be ordered by increasing device sequence.", false)
			return
		}
		if !validateEventKind(w, *event) {
			return
		}
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
	acks := make([]EventAck, 0, len(request.Events))
	for _, event := range request.Events {
		payloadHash, err := hashJSON(event)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Event could not be canonicalized.", false)
			return
		}
		var oldHash []byte
		var oldCursor int64
		err = tx.QueryRowContext(r.Context(), `
			SELECT payload_hash,ack_cursor FROM gateway_events
			WHERE gateway_id=$1 AND event_id=$2`, gatewayID, event.EventID).Scan(&oldHash, &oldCursor)
		if err == nil {
			if !bytesEqual(oldHash, payloadHash) {
				writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Event ID was reused with a different payload.", false)
				return
			}
			acks = append(acks, EventAck{EventID: event.EventID, Cursor: opaqueEventCursor(oldCursor), Duplicate: true})
			continue
		}
		if err != sql.ErrNoRows {
			writeDBUnavailable(w)
			return
		}
		var sequenceExists bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM gateway_events WHERE gateway_id=$1 AND sequence=$2)`, gatewayID, event.Sequence).Scan(&sequenceExists); err != nil {
			writeDBUnavailable(w)
			return
		}
		if sequenceExists {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Gateway sequence was reused for another event.", false)
			return
		}
		eventJSON, err := json.Marshal(event)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Event could not be serialized.", false)
			return
		}
		var ackCursor int64
		if err := tx.QueryRowContext(r.Context(), `
			INSERT INTO gateway_events(owner_id,gateway_id,event_id,sequence,payload_hash,event_json)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING ack_cursor`,
			principal.OwnerID, gatewayID, event.EventID, event.Sequence, payloadHash, string(eventJSON)).Scan(&ackCursor); err != nil {
			if isUniqueViolation(err) {
				writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Event ID or sequence conflicts with another event.", false)
				return
			}
			writeDBUnavailable(w)
			return
		}
		if err := s.applyGatewayEvent(r, tx, principal, event, payloadHash); err != nil {
			if eventErr, ok := err.(eventApplyError); ok {
				writeError(w, eventErr.status, eventErr.code, eventErr.message, eventErr.retryable)
				return
			}
			writeDBUnavailable(w)
			return
		}
		acks = append(acks, EventAck{EventID: event.EventID, Cursor: opaqueEventCursor(ackCursor), Duplicate: false})
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acks": acks})
}

func validateEventKind(w http.ResponseWriter, event GatewayEvent) bool {
	if event.Payload == nil {
		writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "payload is required.", false)
		return false
	}
	switch event.Type {
	case "sms.received":
		if !exactPayloadKeys(event.Payload, []string{"message_id", "from", "text", "parts"}, nil) {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "sms.received payload fields are invalid.", false)
			return false
		}
		if event.SIMID == nil {
			if event.MappingRevision != nil || event.SIMResolution == nil || *event.SIMResolution != "unknown" {
				writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Unknown SIM resolution requires sim_id=null, mapping_revision=null, and sim_resolution=unknown.", false)
				return false
			}
		} else if !validUUID(*event.SIMID) || event.SIMResolution != nil || event.MappingRevision == nil || *event.MappingRevision < 1 {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Resolved SIM events require mapping_revision.", false)
			return false
		}
	case "sms.dispatching", "sms.command_state", "sms.part_state":
		if event.SIMID == nil || !validUUID(*event.SIMID) || event.MappingRevision == nil || *event.MappingRevision < 1 || event.SIMResolution != nil {
			writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "Command events require sim_id and mapping_revision.", false)
			return false
		}
		switch event.Type {
		case "sms.dispatching":
			if !exactPayloadKeys(event.Payload, []string{"message_id", "command_id", "part_count"}, nil) {
				writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "sms.dispatching payload fields are invalid.", false)
				return false
			}
		case "sms.command_state":
			if !exactPayloadKeys(event.Payload, []string{"message_id", "command_id", "state"}, []string{"error"}) {
				writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "sms.command_state payload fields are invalid.", false)
				return false
			}
		case "sms.part_state":
			if !exactPayloadKeys(event.Payload, []string{"message_id", "command_id", "part_index", "state"}, []string{"result_code", "error"}) {
				writeError(w, http.StatusBadRequest, "INVALID_GATEWAY_EVENT", "sms.part_state payload fields are invalid.", false)
				return false
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_EVENT_TYPE", "Event type is not supported by this server version.", false)
		return false
	}
	return true
}

func exactPayloadKeys(payload map[string]any, required, optional []string) bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
		if _, ok := payload[key]; !ok {
			return false
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range payload {
		if !allowed[key] {
			return false
		}
	}
	return true
}

type eventApplyError struct {
	status    int
	code      string
	message   string
	retryable bool
}

func (e eventApplyError) Error() string { return e.message }

func conflictEvent(code, message string, retryable bool) error {
	return eventApplyError{status: http.StatusConflict, code: code, message: message, retryable: retryable}
}

func badEvent(message string) error {
	return eventApplyError{status: http.StatusBadRequest, code: "INVALID_GATEWAY_EVENT", message: message}
}

func (s *Server) applyGatewayEvent(r *http.Request, tx *sql.Tx, principal Principal, event GatewayEvent, payloadHash []byte) error {
	switch event.Type {
	case "sms.received":
		return s.applySMSReceived(r, tx, principal, event, payloadHash)
	case "sms.dispatching":
		return s.applySMSDispatching(r, tx, principal, event)
	case "sms.command_state":
		return s.applySMSCommandState(r, tx, principal, event)
	case "sms.part_state":
		return s.applySMSPartState(r, tx, principal, event)
	default:
		return badEvent("Unsupported event type.")
	}
}

func (s *Server) applySMSReceived(r *http.Request, tx *sql.Tx, principal Principal, event GatewayEvent, eventHash []byte) error {
	messageID, ok := payloadString(event.Payload, "message_id")
	if !ok || !validUUID(messageID) {
		return badEvent("sms.received requires a UUID message_id.")
	}
	from, ok := payloadString(event.Payload, "from")
	if !ok || from == "" || len(from) > 256 || strings.ContainsAny(from, "\x00\r\n") {
		return badEvent("sms.received sender is invalid.")
	}
	text, ok := payloadString(event.Payload, "text")
	if !ok || !validInboundText(text) {
		return badEvent("sms.received text exceeds the byte limit or contains invalid data.")
	}
	partCount, ok := payloadInt(event.Payload, "parts")
	if !ok || partCount < 1 || partCount > 255 {
		return badEvent("sms.received parts must be between 1 and 255.")
	}
	messageHash, _ := hashJSON(struct {
		GatewayID string  `json:"gateway_id"`
		SIMID     *string `json:"sim_id"`
		Revision  *int64  `json:"mapping_revision"`
		From      string  `json:"from"`
		Text      string  `json:"text"`
		Parts     int     `json:"parts"`
	}{event.GatewayID, event.SIMID, event.MappingRevision, from, text, partCount})
	var existingHash []byte
	err := tx.QueryRowContext(r.Context(), `SELECT source_payload_hash FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&existingHash)
	if err == nil {
		if !bytesEqual(existingHash, messageHash) {
			return conflictEvent("IDEMPOTENCY_CONFLICT", "Message ID was reused with different inbound SMS content.", false)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if event.SIMID != nil {
		var found bool
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM sim_bindings WHERE sim_id=$1 AND gateway_id=$2
			AND owner_id=$3 AND mapping_revision >= $4)`, *event.SIMID, event.GatewayID,
			principal.OwnerID, *event.MappingRevision).Scan(&found); err != nil {
			return err
		}
		if !found {
			return conflictEvent("SIM_MAPPING_CHANGED", "Inbound event refers to a SIM mapping not issued to this gateway.", false)
		}
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO messages(id,owner_id,gateway_id,sim_id,mapping_revision,direction,
		from_address,body,status,part_count,source_payload_hash,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,'inbound',$6,$7,'received',$8,$9,$10,$10)`,
		messageID, principal.OwnerID, event.GatewayID, event.SIMID, event.MappingRevision,
		from, text, partCount, messageHash, event.OccurredAt.UTC()); err != nil {
		return err
	}
	return s.addClientEvent(r.Context(), tx, principal.OwnerID, messageID, "message.received")
}

func (s *Server) applySMSDispatching(r *http.Request, tx *sql.Tx, principal Principal, event GatewayEvent) error {
	messageID, commandID, partCount, err := commandEventIDs(event.Payload)
	if err != nil {
		return err
	}
	if partCount < 1 || partCount > 255 {
		return badEvent("sms.dispatching part_count must be between 1 and 255.")
	}
	var state, simID string
	var revision int64
	var ownerID string
	err = tx.QueryRowContext(r.Context(), `
		SELECT c.status,c.sim_id::text,c.mapping_revision,c.owner_id::text
		FROM commands c WHERE c.id=$1 AND c.message_id=$2 AND c.gateway_id=$3 FOR UPDATE`,
		commandID, messageID, event.GatewayID).Scan(&state, &simID, &revision, &ownerID)
	if err == sql.ErrNoRows {
		return conflictEvent("COMMAND_NOT_FOUND", "sms.dispatching does not match a command for this gateway.", false)
	}
	if err != nil {
		return err
	}
	if ownerID != principal.OwnerID || event.SIMID == nil || simID != *event.SIMID || event.MappingRevision == nil || revision != *event.MappingRevision {
		return conflictEvent("SIM_MAPPING_CHANGED", "sms.dispatching SIM or revision does not match the command.", false)
	}
	var existingCount sql.NullInt64
	var messageState string
	if err := tx.QueryRowContext(r.Context(), `SELECT part_count,status FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&existingCount, &messageState); err != nil {
		return err
	}
	if existingCount.Valid && existingCount.Int64 != int64(partCount) {
		return conflictEvent("IDEMPOTENCY_CONFLICT", "sms.dispatching changed the modem-derived part count.", false)
	}
	if state == "queued" {
		return conflictEvent("COMMAND_NOT_CLAIMED", "sms.dispatching was received before the command was claimed.", false)
	}
	if state == "expired" || state == "failed" || state == "delivered" || state == "submitted" {
		if existingCount.Valid {
			return nil
		}
		return conflictEvent("COMMAND_TERMINAL", "A terminal command cannot begin dispatching.", false)
	}
	if !existingCount.Valid {
		if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET part_count=$1,updated_at=now() WHERE id=$2`, partCount, messageID); err != nil {
			return err
		}
		for index := 0; index < partCount; index++ {
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO message_parts(message_id,part_index,state) VALUES ($1,$2,'dispatching')`, messageID, index); err != nil {
				return err
			}
		}
	}
	if state == "accepted_by_gateway" {
		if _, err := tx.ExecContext(r.Context(), `UPDATE commands SET status='dispatching',updated_at=now() WHERE id=$1`, commandID); err != nil {
			return err
		}
	}
	if messageState == "accepted_by_gateway" {
		if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET status='dispatching',updated_at=now() WHERE id=$1`, messageID); err != nil {
			return err
		}
		return s.addClientEvent(r.Context(), tx, principal.OwnerID, messageID, "message.state_changed")
	}
	return nil
}

func (s *Server) applySMSCommandState(r *http.Request, tx *sql.Tx, principal Principal, event GatewayEvent) error {
	messageID, commandID, _, err := commandEventIDs(event.Payload)
	if err != nil {
		return err
	}
	stateValue, ok := payloadString(event.Payload, "state")
	if !ok || stateValue != "expired" && stateValue != "failed" && stateValue != "unknown" {
		return badEvent("sms.command_state state must be expired, failed, or unknown.")
	}
	errorValue, _ := payloadString(event.Payload, "error")
	if len(errorValue) > 512 || strings.ContainsRune(errorValue, '\x00') {
		return badEvent("sms.command_state error is too long or invalid.")
	}
	var commandStatus, simID, ownerID string
	var revision int64
	err = tx.QueryRowContext(r.Context(), `
		SELECT status,sim_id::text,mapping_revision,owner_id::text FROM commands
		WHERE id=$1 AND message_id=$2 AND gateway_id=$3 FOR UPDATE`, commandID, messageID, event.GatewayID).
		Scan(&commandStatus, &simID, &revision, &ownerID)
	if err == sql.ErrNoRows {
		return conflictEvent("COMMAND_NOT_FOUND", "sms.command_state does not match a command for this gateway.", false)
	}
	if err != nil {
		return err
	}
	if ownerID != principal.OwnerID || event.SIMID == nil || simID != *event.SIMID || event.MappingRevision == nil || revision != *event.MappingRevision {
		return conflictEvent("SIM_MAPPING_CHANGED", "sms.command_state SIM or revision does not match the command.", false)
	}
	var partCount sql.NullInt64
	var current string
	if err := tx.QueryRowContext(r.Context(), `SELECT part_count,status FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&partCount, &current); err != nil {
		return err
	}
	if current == "delivered" || current == "failed" || current == "expired" || commandStatus == "delivered" || commandStatus == "failed" || commandStatus == "expired" {
		return nil
	}
	if (stateValue == "failed" || stateValue == "expired") && partCount.Valid {
		// Once the modem-side part count is known, a command-level terminal
		// event cannot erase per-part evidence.
		return nil
	}
	if stateValue == "expired" || stateValue == "failed" {
		if current != "queued" && current != "accepted_by_gateway" {
			return nil
		}
	}
	if stateValue == "unknown" {
		if current == "submitted" || current == "delivered" {
			return nil
		}
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE commands SET status=$1,updated_at=now() WHERE id=$2`, stateValue, commandID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE messages SET status=$1,updated_at=now() WHERE id=$2`, stateValue, messageID); err != nil {
		return err
	}
	if stateValue != current {
		if errorValue != "" {
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO audit_log(owner_id,action,resource_id,details)
				VALUES ($1,'sms.command_state_reported',$2,jsonb_build_object('state',$3,'error',$4))`,
				principal.OwnerID, messageID, stateValue, errorValue); err != nil {
				return err
			}
		}
		return s.addClientEvent(r.Context(), tx, principal.OwnerID, messageID, "message.state_changed")
	}
	return nil
}

func (s *Server) applySMSPartState(r *http.Request, tx *sql.Tx, principal Principal, event GatewayEvent) error {
	messageID, commandID, _, err := commandEventIDs(event.Payload)
	if err != nil {
		return err
	}
	partIndex, ok := payloadInt(event.Payload, "part_index")
	if !ok || partIndex < 0 || partIndex > 254 {
		return badEvent("sms.part_state part_index must be a zero-based integer.")
	}
	stateValue, ok := payloadString(event.Payload, "state")
	if !ok || stateValue != "submitted" && stateValue != "delivered" && stateValue != "failed" && stateValue != "expired" && stateValue != "unknown" {
		return badEvent("sms.part_state state is unsupported.")
	}
	var resultCode *int
	if raw, present := event.Payload["result_code"]; present && raw != nil {
		value, ok := numberToInt(raw)
		if !ok {
			return badEvent("sms.part_state result_code must be an integer.")
		}
		resultCode = &value
	}
	errorValue, _ := payloadString(event.Payload, "error")
	if len(errorValue) > 512 || strings.ContainsRune(errorValue, '\x00') {
		return badEvent("sms.part_state error is too long or invalid.")
	}
	var simID, commandStatus string
	var revision int64
	err = tx.QueryRowContext(r.Context(), `
		SELECT sim_id::text,mapping_revision,status FROM commands
		WHERE id=$1 AND message_id=$2 AND gateway_id=$3 FOR UPDATE`, commandID, messageID, event.GatewayID).
		Scan(&simID, &revision, &commandStatus)
	if err == sql.ErrNoRows {
		return conflictEvent("COMMAND_NOT_FOUND", "sms.part_state does not match a command for this gateway.", false)
	}
	if err != nil {
		return err
	}
	if event.SIMID == nil || simID != *event.SIMID || event.MappingRevision == nil || revision != *event.MappingRevision {
		return conflictEvent("SIM_MAPPING_CHANGED", "sms.part_state SIM or revision does not match the command.", false)
	}
	var partCount sql.NullInt64
	if err := tx.QueryRowContext(r.Context(), `SELECT part_count FROM messages WHERE id=$1`, messageID).Scan(&partCount); err != nil {
		return err
	}
	if !partCount.Valid || int64(partIndex) >= partCount.Int64 {
		return conflictEvent("EVENT_ORDER_REQUIRED", "sms.dispatching with modem-derived part_count must commit before part callbacks.", true)
	}
	var currentState string
	var oldResult sql.NullInt64
	var oldError sql.NullString
	err = tx.QueryRowContext(r.Context(), `SELECT state,result_code,error FROM message_parts WHERE message_id=$1 AND part_index=$2 FOR UPDATE`, messageID, partIndex).
		Scan(&currentState, &oldResult, &oldError)
	if err == sql.ErrNoRows {
		return conflictEvent("EVENT_ORDER_REQUIRED", "Part ledger is not initialized by sms.dispatching.", true)
	}
	if err != nil {
		return err
	}
	partChanged := false
	if partStateCanAdvance(currentState, stateValue) {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE message_parts SET state=$1,result_code=$2,error=$3,updated_at=now()
			WHERE message_id=$4 AND part_index=$5`, stateValue, resultCode, optionalString(errorValue), messageID, partIndex); err != nil {
			return err
		}
		partChanged = true
	} else if stateValue == currentState && (!oldResult.Valid && resultCode != nil || !oldError.Valid && errorValue != "") {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE message_parts SET result_code=COALESCE(result_code,$1),error=COALESCE(error,$2),updated_at=now()
			WHERE message_id=$3 AND part_index=$4`, resultCode, optionalString(errorValue), messageID, partIndex); err != nil {
			return err
		}
		partChanged = true
	}
	return s.aggregateParts(r.Context(), tx, commandID, messageID, principal.OwnerID, partChanged)
}

func partStateCanAdvance(current, next string) bool {
	if current == next {
		return false
	}
	switch current {
	case "dispatching":
		return next == "submitted" || next == "delivered" || next == "failed" || next == "expired" || next == "unknown"
	case "unknown":
		return next == "submitted" || next == "delivered" || next == "failed"
	case "submitted":
		return next == "delivered" || next == "failed"
	default:
		return false
	}
}

func commandEventIDs(payload map[string]any) (string, string, int, error) {
	messageID, ok := payloadString(payload, "message_id")
	if !ok || !validUUID(messageID) {
		return "", "", 0, badEvent("Command event requires a UUID message_id.")
	}
	commandID, ok := payloadString(payload, "command_id")
	if !ok || !validUUID(commandID) {
		return "", "", 0, badEvent("Command event requires a UUID command_id.")
	}
	partCount := 0
	if value, exists := payload["part_count"]; exists {
		partCount, ok = numberToInt(value)
		if !ok {
			return "", "", 0, badEvent("Command event part_count must be an integer.")
		}
	}
	return strings.ToLower(messageID), strings.ToLower(commandID), partCount, nil
}

func payloadString(payload map[string]any, key string) (string, bool) {
	value, ok := payload[key].(string)
	return value, ok
}

func payloadInt(payload map[string]any, key string) (int, bool) {
	value, exists := payload[key]
	if !exists {
		return 0, false
	}
	return numberToInt(value)
}

func numberToInt(value any) (int, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil && int64(int(parsed)) == parsed
	case int:
		return number, true
	case int64:
		return int(number), int64(int(number)) == number
	case float64:
		integer := int(number)
		return integer, float64(integer) == number
	default:
		return 0, false
	}
}

func validInboundText(value string) bool {
	return len([]byte(value)) <= 16384 && !strings.ContainsRune(value, '\x00')
}
