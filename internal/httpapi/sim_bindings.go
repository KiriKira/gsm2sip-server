package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

type simBindingEnvelope struct {
	OperationID   string            `json:"operation_id"`
	Phase         string            `json:"phase"`
	Mappings      []SIMProposal     `json:"mappings"`
	Confirmations []SIMConfirmation `json:"confirmations"`
}

func (s *Server) updateSIMBindings(w http.ResponseWriter, r *http.Request, principal Principal) {
	if !ownsGatewayPath(w, r, principal) {
		return
	}
	if principal.Role != "gateway" {
		writeError(w, http.StatusForbidden, "GATEWAY_REQUIRED", "Only the paired gateway can report SIM bindings.", false)
		return
	}
	gatewayID := strings.ToLower(r.PathValue("gateway_id"))
	var request simBindingEnvelope
	if !decodeOrError(w, r, &request, 32*1024) {
		return
	}
	request.OperationID = strings.ToLower(request.OperationID)
	if !validUUID(request.OperationID) {
		writeError(w, http.StatusBadRequest, "INVALID_OPERATION_ID", "operation_id must be a UUID.", false)
		return
	}
	switch request.Phase {
	case "propose":
		s.proposeSIMBindings(w, r, principal, gatewayID, request)
	case "confirm":
		s.confirmSIMBindings(w, r, principal, gatewayID, request)
	default:
		writeError(w, http.StatusBadRequest, "INVALID_PHASE", "phase must be propose or confirm.", false)
	}
}

func (s *Server) proposeSIMBindings(w http.ResponseWriter, r *http.Request, principal Principal, gatewayID string, request simBindingEnvelope) {
	if len(request.Mappings) < 1 || len(request.Mappings) > 2 {
		writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "A proposal must describe one or two populated SIM slots.", false)
		return
	}
	seenSlots, seenSIMs := map[int]bool{}, map[string]bool{}
	for i := range request.Mappings {
		mapping := &request.Mappings[i]
		mapping.Label = strings.TrimSpace(mapping.Label)
		if mapping.SlotIndex < 0 || mapping.SlotIndex > 1 || seenSlots[mapping.SlotIndex] || mapping.Label == "" || len(mapping.Label) > 64 {
			writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "SIM slots must be unique and labels must be between 1 and 64 characters.", false)
			return
		}
		seenSlots[mapping.SlotIndex] = true
		if mapping.CarrierName != nil && len(*mapping.CarrierName) > 100 || mapping.PhoneNumber != nil && len(*mapping.PhoneNumber) > 64 {
			writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "SIM carrier or phone number is too long.", false)
			return
		}
		if mapping.ExistingSIMID != nil {
			*mapping.ExistingSIMID = strings.ToLower(*mapping.ExistingSIMID)
			if !validUUID(*mapping.ExistingSIMID) || !mapping.SameSIMVerified || seenSIMs[*mapping.ExistingSIMID] {
				writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "A reused SIM ID requires a locally verified identity and may appear once.", false)
				return
			}
			seenSIMs[*mapping.ExistingSIMID] = true
		} else if mapping.SameSIMVerified {
			writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "same_sim_verified requires an existing_sim_id.", false)
			return
		}
	}
	hash, err := hashJSON(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SIM_MAPPINGS", "SIM mappings are invalid.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	var currentRevision int64
	err = tx.QueryRowContext(r.Context(), `
		SELECT g.mapping_revision FROM gateways g JOIN devices d ON d.id=g.device_id
		WHERE g.device_id=$1 AND d.owner_id=$2 AND d.state='active' FOR UPDATE OF g`, gatewayID, principal.OwnerID).Scan(&currentRevision)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	var previousHash []byte
	var previousResult []byte
	err = tx.QueryRowContext(r.Context(), `
		SELECT proposal_hash, proposal_result FROM sim_binding_operations
		WHERE gateway_id=$1 AND operation_id=$2`, gatewayID, request.OperationID).Scan(&previousHash, &previousResult)
	if err == nil {
		if !bytesEqual(hash, previousHash) {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Operation ID was reused with different SIM mappings.", false)
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBUnavailable(w)
			return
		}
		writeRawJSON(w, http.StatusOK, previousResult)
		return
	}
	if err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	for _, mapping := range request.Mappings {
		if mapping.ExistingSIMID == nil {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM sim_bindings WHERE sim_id=$1 AND gateway_id=$2 AND owner_id=$3)`,
			*mapping.ExistingSIMID, gatewayID, principal.OwnerID).Scan(&exists); err != nil {
			writeDBUnavailable(w)
			return
		}
		if !exists {
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "The reused SIM ID is not owned by this gateway.", false)
			return
		}
	}
	newRevision := currentRevision + 1
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE sim_bindings SET state='removed', identity_verified=false, updated_at=now()
		WHERE gateway_id=$1 AND state IN ('pending_local_confirmation','active','unverified')`, gatewayID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE gateways SET mapping_revision=$1 WHERE device_id=$2`, newRevision, gatewayID); err != nil {
		writeDBUnavailable(w)
		return
	}
	result := SIMBindingResult{OperationID: request.OperationID, Phase: "proposed", MappingRevision: newRevision, Mappings: make([]SIMBindingSummary, 0, len(request.Mappings))}
	for _, mapping := range request.Mappings {
		simID := newUUID()
		if mapping.ExistingSIMID != nil {
			simID = *mapping.ExistingSIMID
			_, err = tx.ExecContext(r.Context(), `
				UPDATE sim_bindings SET slot_index=$1, label=$2, carrier_name=$3,
				phone_number=$4, state='pending_local_confirmation', identity_verified=false,
				mapping_revision=$5, service_state=NULL, updated_at=now()
				WHERE sim_id=$6 AND gateway_id=$7 AND owner_id=$8`, mapping.SlotIndex,
				mapping.Label, mapping.CarrierName, mapping.PhoneNumber, newRevision, simID, gatewayID, principal.OwnerID)
		} else {
			_, err = tx.ExecContext(r.Context(), `
				INSERT INTO sim_bindings(sim_id, owner_id, gateway_id, slot_index, label,
				carrier_name, phone_number, state, identity_verified, mapping_revision)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'pending_local_confirmation',false,$8)`, simID,
				principal.OwnerID, gatewayID, mapping.SlotIndex, mapping.Label,
				mapping.CarrierName, mapping.PhoneNumber, newRevision)
		}
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		result.Mappings = append(result.Mappings, SIMBindingSummary{SIMID: simID, SlotIndex: mapping.SlotIndex, State: "pending_local_confirmation", MappingRevision: newRevision})
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO sim_binding_operations(gateway_id, operation_id, proposal_hash, proposal_result)
		VALUES ($1,$2,$3,$4)`, gatewayID, request.OperationID, hash, string(resultJSON)); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Operation ID was used concurrently.", false)
			return
		}
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO audit_log(owner_id, actor_device_id, action, resource_id, details)
		VALUES ($1,$2,'sim.binding_proposed',$2,jsonb_build_object('mapping_revision',$3))`,
		principal.OwnerID, principal.DeviceID, newRevision); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) confirmSIMBindings(w http.ResponseWriter, r *http.Request, principal Principal, gatewayID string, request simBindingEnvelope) {
	if len(request.Confirmations) < 1 || len(request.Confirmations) > 2 {
		writeError(w, http.StatusBadRequest, "INVALID_SIM_CONFIRMATION", "Confirmation must include each proposed SIM mapping.", false)
		return
	}
	for i := range request.Confirmations {
		request.Confirmations[i].SIMID = strings.ToLower(request.Confirmations[i].SIMID)
		if !validUUID(request.Confirmations[i].SIMID) || request.Confirmations[i].SlotIndex < 0 || request.Confirmations[i].SlotIndex > 1 {
			writeError(w, http.StatusBadRequest, "INVALID_SIM_CONFIRMATION", "SIM confirmation fields are invalid.", false)
			return
		}
	}
	hash, err := hashJSON(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SIM_CONFIRMATION", "SIM confirmation fields are invalid.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	var currentRevision int64
	err = tx.QueryRowContext(r.Context(), `SELECT mapping_revision FROM gateways WHERE device_id=$1 FOR UPDATE`, gatewayID).Scan(&currentRevision)
	if err == sql.ErrNoRows {
		writeNotFound(w)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	var proposalJSON, previousConfirmationHash, previousResult []byte
	err = tx.QueryRowContext(r.Context(), `
		SELECT proposal_result, confirmation_hash, confirmation_result
		FROM sim_binding_operations WHERE gateway_id=$1 AND operation_id=$2 FOR UPDATE`, gatewayID, request.OperationID).
		Scan(&proposalJSON, &previousConfirmationHash, &previousResult)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "SIM proposal does not exist or expired.", false)
		return
	}
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if previousConfirmationHash != nil {
		if !bytesEqual(hash, previousConfirmationHash) {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Operation ID was confirmed with different results.", false)
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBUnavailable(w)
			return
		}
		writeRawJSON(w, http.StatusOK, previousResult)
		return
	}
	var proposal SIMBindingResult
	if err := json.Unmarshal(proposalJSON, &proposal); err != nil {
		writeDBUnavailable(w)
		return
	}
	if proposal.MappingRevision != currentRevision || len(proposal.Mappings) != len(request.Confirmations) {
		writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "A newer SIM mapping was proposed or not all SIMs were confirmed.", false)
		return
	}
	confirmations := make(map[string]SIMConfirmation, len(request.Confirmations))
	for _, confirmation := range request.Confirmations {
		if _, duplicate := confirmations[confirmation.SIMID]; duplicate {
			writeError(w, http.StatusBadRequest, "INVALID_SIM_CONFIRMATION", "A SIM may only be confirmed once.", false)
			return
		}
		confirmations[confirmation.SIMID] = confirmation
	}
	result := SIMBindingResult{OperationID: request.OperationID, Phase: "confirmed", MappingRevision: proposal.MappingRevision, Mappings: make([]SIMBindingSummary, 0, len(proposal.Mappings))}
	for _, mapping := range proposal.Mappings {
		confirmation, exists := confirmations[mapping.SIMID]
		if !exists || confirmation.SlotIndex != mapping.SlotIndex {
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "Confirmation does not match the proposed slot mapping.", false)
			return
		}
		state := "unverified"
		verified := false
		if confirmation.Confirmed {
			state = "active"
			verified = true
		}
		res, err := tx.ExecContext(r.Context(), `
			UPDATE sim_bindings SET state=$1, identity_verified=$2, updated_at=now()
			WHERE sim_id=$3 AND gateway_id=$4 AND owner_id=$5
			AND state='pending_local_confirmation' AND mapping_revision=$6`,
			state, verified, mapping.SIMID, gatewayID, principal.OwnerID, proposal.MappingRevision)
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		updated, _ := res.RowsAffected()
		if updated != 1 {
			writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "SIM mapping is no longer pending confirmation.", false)
			return
		}
		result.Mappings = append(result.Mappings, SIMBindingSummary{SIMID: mapping.SIMID, SlotIndex: mapping.SlotIndex, State: state, MappingRevision: proposal.MappingRevision})
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE sim_binding_operations SET confirmation_hash=$1, confirmation_result=$2 WHERE gateway_id=$3 AND operation_id=$4`, hash, string(resultJSON), gatewayID, request.OperationID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO audit_log(owner_id, actor_device_id, action, resource_id, details)
		VALUES ($1,$2,'sim.binding_confirmed',$2,jsonb_build_object('mapping_revision',$3))`,
		principal.OwnerID, principal.DeviceID, proposal.MappingRevision); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeRawJSON(w http.ResponseWriter, status int, payload []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}
