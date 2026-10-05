package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var platformPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func validPlatform(value string) bool {
	return platformPattern.MatchString(value)
}

type ClientSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	State    string `json:"state"`
	IsSelf   bool   `json:"is_self"`
}

func (s *Server) listClients(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a client can list paired clients.", false)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id::text, name, platform, state, id=$2
		FROM devices WHERE owner_id=$1 AND role='client'
		ORDER BY created_at, id`, principal.OwnerID, principal.DeviceID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer rows.Close()
	items := make([]ClientSummary, 0)
	for rows.Next() {
		var item ClientSummary
		if err := rows.Scan(&item.ID, &item.Name, &item.Platform, &item.State, &item.IsSelf); err != nil {
			writeDBUnavailable(w)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type GatewaySummary struct {
	GatewayID       string     `json:"gateway_id"`
	DeviceName      string     `json:"device_name"`
	Online          bool       `json:"online"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	MappingRevision int64      `json:"mapping_revision"`
	Sequence        *int64     `json:"sequence"`
	ProtocolVersion *int       `json:"protocol_version"`
	AppVersion      *string    `json:"app_version"`
	Root            *bool      `json:"root"`
	SIPRegistered   *bool      `json:"sip_registered"`
	BatteryPercent  *int       `json:"battery_percent"`
	Charging        *bool      `json:"charging"`
}

type SIMSummary struct {
	SIMID            string  `json:"sim_id"`
	SlotIndex        int     `json:"slot_index"`
	Label            string  `json:"label"`
	CarrierName      *string `json:"carrier_name"`
	PhoneNumber      *string `json:"phone_number"`
	State            string  `json:"state"`
	MappingRevision  int64   `json:"mapping_revision"`
	IdentityVerified bool    `json:"identity_verified"`
	ServiceState     *string `json:"service_state"`
}

func (s *Server) listGateways(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" && principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "This device role cannot list gateways.", false)
		return
	}
	query := `
		SELECT g.device_id::text, d.name, g.last_seen_at, g.mapping_revision,
		g.heartbeat_sequence, g.protocol_version, g.app_version, g.rooted,
		g.sip_registered, g.battery_percent, g.charging,
		(g.last_seen_at IS NOT NULL AND g.last_seen_at > now() - interval '90 seconds')
		FROM gateways g JOIN devices d ON d.id=g.device_id
		WHERE d.owner_id=$1 AND d.state='active'`
	args := []any{principal.OwnerID}
	if principal.Role == "gateway" {
		query += ` AND g.device_id=$2`
		args = append(args, principal.DeviceID)
	}
	query += ` ORDER BY d.created_at`
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer rows.Close()
	items := make([]GatewaySummary, 0)
	for rows.Next() {
		var item GatewaySummary
		var lastSeen sql.NullTime
		var sequence sql.NullInt64
		var protocol sql.NullInt64
		var appVersion sql.NullString
		var battery sql.NullInt64
		var rooted, sipRegistered, charging sql.NullBool
		if err := rows.Scan(&item.GatewayID, &item.DeviceName, &lastSeen, &item.MappingRevision,
			&sequence, &protocol, &appVersion, &rooted, &sipRegistered, &battery, &charging, &item.Online); err != nil {
			writeDBUnavailable(w)
			return
		}
		if lastSeen.Valid {
			value := lastSeen.Time
			item.LastSeenAt = &value
		}
		item.Sequence = nullableInt64(sequence)
		if protocol.Valid {
			value := int(protocol.Int64)
			item.ProtocolVersion = &value
		}
		item.AppVersion = nullableString(appVersion)
		if rooted.Valid {
			value := rooted.Bool
			item.Root = &value
		}
		if sipRegistered.Valid {
			value := sipRegistered.Bool
			item.SIPRegistered = &value
		}
		if battery.Valid {
			value := int(battery.Int64)
			item.BatteryPercent = &value
		}
		if charging.Valid {
			value := charging.Bool
			item.Charging = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) listSIMs(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	var revision int64
	err := s.db.QueryRowContext(r.Context(), `
		SELECT g.mapping_revision FROM gateways g JOIN devices d ON d.id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.state='active'`, gatewayID, principal.OwnerID).Scan(&revision)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT sim_id::text, slot_index, label, carrier_name, phone_number, state,
		mapping_revision, identity_verified, service_state
		FROM sim_bindings WHERE gateway_id=$1 AND owner_id=$2
		ORDER BY slot_index, created_at`, gatewayID, principal.OwnerID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer rows.Close()
	sims := make([]SIMSummary, 0, 2)
	for rows.Next() {
		var item SIMSummary
		var carrier, number, service sql.NullString
		if err := rows.Scan(&item.SIMID, &item.SlotIndex, &item.Label, &carrier, &number,
			&item.State, &item.MappingRevision, &item.IdentityVerified, &service); err != nil {
			writeDBUnavailable(w)
			return
		}
		item.CarrierName = nullableString(carrier)
		item.PhoneNumber = nullableString(number)
		item.ServiceState = nullableString(service)
		sims = append(sims, item)
	}
	if err := rows.Err(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mapping_revision": revision, "sims": sims})
}

func ownsGatewayPath(w http.ResponseWriter, r *http.Request, principal Principal) bool {
	pathID := strings.ToLower(r.PathValue("gateway_id"))
	if !validUUID(pathID) {
		writeNotFound(w)
		return false
	}
	if principal.Role != "gateway" && principal.Role != "client" {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN", "This device role cannot access gateway state.", false)
		return false
	}
	if principal.Role == "gateway" && pathID != strings.ToLower(principal.DeviceID) {
		writeNotFound(w)
		return false
	}
	return true
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	if principal.Role != "gateway" || gatewayID != strings.ToLower(principal.DeviceID) {
		writeError(w, http.StatusForbidden, "GATEWAY_REQUIRED", "Only this paired gateway can report its heartbeat.", false)
		return
	}
	var request HeartbeatRequest
	if !decodeOrError(w, r, &request, 64*1024) {
		return
	}
	if request.Sequence < 1 || request.ProtocolVersion != 1 || strings.TrimSpace(request.AppVersion) == "" || len(request.AppVersion) > 64 || len(request.SIMStates) > 2 {
		writeError(w, http.StatusBadRequest, "INVALID_HEARTBEAT", "Heartbeat fields are invalid.", false)
		return
	}
	if request.BatteryPercent != nil && (*request.BatteryPercent < 0 || *request.BatteryPercent > 100) {
		writeError(w, http.StatusBadRequest, "INVALID_HEARTBEAT", "Battery percentage must be between 0 and 100.", false)
		return
	}
	seen := map[string]bool{}
	for _, state := range request.SIMStates {
		state.SIMID = strings.ToLower(state.SIMID)
		if !validUUID(state.SIMID) || seen[state.SIMID] || state.MappingRevision < 1 || state.ServiceState == "" || len(state.ServiceState) > 32 {
			writeError(w, http.StatusBadRequest, "INVALID_HEARTBEAT", "SIM heartbeat fields are invalid.", false)
			return
		}
		seen[state.SIMID] = true
	}
	hash, err := hashJSON(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_HEARTBEAT", "Heartbeat fields are invalid.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	var currentSequence sql.NullInt64
	var currentHash, previousResult []byte
	var currentRevision int64
	err = tx.QueryRowContext(r.Context(), `
		SELECT heartbeat_sequence,heartbeat_hash,heartbeat_result,mapping_revision
		FROM gateways WHERE device_id=$1 FOR UPDATE`, gatewayID).Scan(&currentSequence, &currentHash, &previousResult, &currentRevision)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if currentSequence.Valid {
		switch {
		case request.Sequence < currentSequence.Int64:
			writeError(w, http.StatusConflict, "STALE_HEARTBEAT", "Heartbeat sequence is older than the latest committed update.", false)
			return
		case request.Sequence == currentSequence.Int64:
			if !bytesEqual(hash, currentHash) {
				writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Heartbeat sequence was reused with a different payload.", false)
				return
			}
			_ = tx.Rollback()
			writeRawJSON(w, http.StatusOK, previousResult)
			return
		}
	}
	type currentSIM struct {
		revision int64
		state    string
		verified bool
		slot     int
	}
	currentSIMs := make(map[string]currentSIM)
	rows, err := tx.QueryContext(r.Context(), `
		SELECT sim_id::text,mapping_revision,state,identity_verified,slot_index
		FROM sim_bindings WHERE gateway_id=$1 AND owner_id=$2
		AND state IN ('pending_local_confirmation','active','unverified') ORDER BY slot_index FOR UPDATE`, gatewayID, principal.OwnerID)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	for rows.Next() {
		var id string
		var item currentSIM
		if err := rows.Scan(&id, &item.revision, &item.state, &item.verified, &item.slot); err != nil {
			_ = rows.Close()
			writeDBUnavailable(w)
			return
		}
		currentSIMs[id] = item
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	reported := make(map[string]SIMState, len(request.SIMStates))
	for _, state := range request.SIMStates {
		current, exists := currentSIMs[state.SIMID]
		if !exists || state.MappingRevision != currentRevision || current.revision != currentRevision {
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "Heartbeat does not match the current server-issued SIM revision; fetch /gateways/{id}/sims and resync.", false)
			return
		}
		if state.IdentityVerified && (current.state != "active" || !current.verified) {
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "Heartbeat cannot activate an unverified SIM; use the local confirmation flow.", false)
			return
		}
		reported[state.SIMID] = state
	}
	invalidated := make([]string, 0)
	for simID, current := range currentSIMs {
		state, present := reported[simID]
		serviceState := "absent"
		if present {
			serviceState = state.ServiceState
		}
		shouldInvalidate := current.state == "active" && (!present || !state.IdentityVerified)
		if shouldInvalidate {
			invalidated = append(invalidated, simID)
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE sim_bindings SET state='unverified',identity_verified=false,
				service_state=$1,updated_at=now() WHERE sim_id=$2`, serviceState, simID); err != nil {
				writeDBUnavailable(w)
				return
			}
		} else if present {
			if _, err := tx.ExecContext(r.Context(), `UPDATE sim_bindings SET service_state=$1,updated_at=now() WHERE sim_id=$2`, serviceState, simID); err != nil {
				writeDBUnavailable(w)
				return
			}
		}
	}
	newRevision := currentRevision
	if len(invalidated) > 0 {
		newRevision++
		if _, err := tx.ExecContext(r.Context(), `UPDATE gateways SET mapping_revision=$1 WHERE device_id=$2`, newRevision, gatewayID); err != nil {
			writeDBUnavailable(w)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE sim_bindings SET mapping_revision=$1,updated_at=now()
			WHERE gateway_id=$2 AND state IN ('pending_local_confirmation','active','unverified')`, newRevision, gatewayID); err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	response := HeartbeatResponse{ServerTime: s.now().UTC(), Online: true, StaleAfterSeconds: 90, MappingRevision: newRevision, InvalidatedSIMIDs: invalidated}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE gateways SET heartbeat_sequence=$1, heartbeat_hash=$2, heartbeat_result=$3, last_seen_at=now(),
		protocol_version=$4, app_version=$5, rooted=$6, sip_registered=$7,
		battery_percent=$8, charging=$9 WHERE device_id=$10`, request.Sequence, hash, string(responseJSON),
		request.ProtocolVersion, request.AppVersion, request.Root, request.SIPRegistered,
		request.BatteryPercent, request.Charging, gatewayID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeRawJSON(w, http.StatusOK, responseJSON)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", "Resource does not exist.", false)
}
