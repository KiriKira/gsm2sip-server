package httpapi

import (
	"errors"
	"net/http"
	"strings"

	callsvc "github.com/kirikira/gsm2sip-server/internal/calls"
)

type createCallIntentBody struct {
	GatewayID       string `json:"gateway_id"`
	SIMID           string `json:"sim_id"`
	MappingRevision int64  `json:"mapping_revision"`
	To              string `json:"to"`
}

type callReadyBody struct {
	CallID    string `json:"call_id"`
	WakeNonce string `json:"wake_nonce"`
}

func (s *Server) createCallIntent(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can create a call intent.", false)
		return
	}
	if s.calls == nil || !s.calls.Configured() {
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
		return
	}
	var body createCallIntentBody
	if !decodeOrError(w, r, &body, 16*1024) {
		return
	}
	body.GatewayID = strings.ToLower(body.GatewayID)
	body.SIMID = strings.ToLower(body.SIMID)
	if !validUUID(body.GatewayID) || !validUUID(body.SIMID) || body.MappingRevision < 1 || !validAddress(body.To) {
		writeError(w, http.StatusBadRequest, "INVALID_CALL_INTENT", "Gateway, SIM, mapping revision, or destination is invalid.", false)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !visibleIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 visible characters.", false)
		return
	}
	intent, err := s.calls.CreateIntent(r.Context(), principal.OwnerID, principal.DeviceID, callsvc.CreateIntentRequest{
		GatewayID: body.GatewayID, SIMID: body.SIMID, MappingRevision: body.MappingRevision, To: body.To,
	}, key)
	if err != nil {
		writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, intent)
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can read call records.", false)
		return
	}
	if s.calls == nil {
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
		return
	}
	if len(r.URL.Query()["cursor"]) > 1 || len(r.URL.Query()["limit"]) > 1 {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "Call query parameters may appear once.", false)
		return
	}
	items, next, err := s.calls.ListCalls(r.Context(), principal.OwnerID, principal.DeviceID, r.URL.Query().Get("cursor"), queryLimit(r))
	if err != nil {
		writeCallError(w, err)
		return
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	writeJSON(w, http.StatusOK, callsvc.ListResult{Items: items, NextCursor: nextCursor, ResyncRequired: false})
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can read call records.", false)
		return
	}
	if s.calls == nil {
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
		return
	}
	callID := strings.ToLower(r.PathValue("call_id"))
	item, err := s.calls.GetCall(r.Context(), principal.OwnerID, principal.DeviceID, callID)
	if err != nil {
		writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) cancelCallIntent(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can cancel a call intent.", false)
		return
	}
	if s.calls == nil {
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
		return
	}
	intentID := strings.ToLower(r.PathValue("intent_id"))
	if err := s.calls.CancelIntent(r.Context(), principal.OwnerID, principal.DeviceID, intentID); err != nil {
		writeCallError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clientReady(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can report call readiness.", false)
		return
	}
	if s.calls == nil || !s.calls.Configured() {
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
		return
	}
	var body callReadyBody
	if !decodeOrError(w, r, &body, 8192) {
		return
	}
	body.CallID = strings.ToLower(body.CallID)
	pathClientID := strings.ToLower(r.PathValue("client_id"))
	item, err := s.calls.ClientReady(r.Context(), principal.OwnerID, principal.DeviceID, pathClientID, body.CallID, body.WakeNonce)
	if err != nil {
		writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, callsvc.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "CALLING_NOT_CONFIGURED", "SIP calling is not configured.", false)
	case errors.Is(err, callsvc.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, "ARI_UNAVAILABLE", "The call coordinator is not ready.", true)
	case errors.Is(err, callsvc.ErrInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_CALL_REQUEST", "Call request fields are invalid.", false)
	case errors.Is(err, callsvc.ErrForbidden):
		writeError(w, http.StatusForbidden, "CALL_FORBIDDEN", "Call resource is not available to this client.", false)
	case errors.Is(err, callsvc.ErrConflict):
		writeError(w, http.StatusConflict, "CALL_STATE_CONFLICT", "Call request conflicts with the current state.", false)
	case errors.Is(err, callsvc.ErrNotFound):
		writeError(w, http.StatusNotFound, "CALL_NOT_FOUND", "Call resource was not found.", false)
	case errors.Is(err, callsvc.ErrGatewayUnavailable):
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UNAVAILABLE", "The gateway SIP endpoint is unavailable.", true)
	case errors.Is(err, callsvc.ErrSIMUnavailable):
		writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "The selected SIM mapping is unavailable or changed.", false)
	case errors.Is(err, callsvc.ErrBusy):
		writeError(w, http.StatusConflict, "GATEWAY_BUSY", "The gateway already has a call in progress.", false)
	default:
		writeDBUnavailable(w)
	}
}
