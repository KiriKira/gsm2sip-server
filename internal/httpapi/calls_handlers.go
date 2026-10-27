package httpapi

import (
	"errors"
	"net/http"
	"strings"

	callsvc "github.com/kirikira/gsm2sip-server/internal/calls"
)

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request, principal Principal) {
	if principal.Role != "client" {
		writeError(w, http.StatusForbidden, "CLIENT_REQUIRED", "Only a paired client can read call records.", false)
		return
	}
	if len(r.URL.Query()["cursor"]) > 1 || len(r.URL.Query()["limit"]) > 1 {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "Call query parameters may appear once.", false)
		return
	}
	items, next, err := s.history.ListCalls(r.Context(), principal.OwnerID, principal.DeviceID, r.URL.Query().Get("cursor"), queryLimit(r))
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
	callID := strings.ToLower(r.PathValue("call_id"))
	item, err := s.history.GetCall(r.Context(), principal.OwnerID, principal.DeviceID, callID)
	if err != nil {
		writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, callsvc.ErrInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_CALL_REQUEST", "Call request fields are invalid.", false)
	case errors.Is(err, callsvc.ErrForbidden):
		writeError(w, http.StatusForbidden, "CALL_FORBIDDEN", "Call resource is not available to this client.", false)
	case errors.Is(err, callsvc.ErrConflict):
		writeError(w, http.StatusConflict, "CALL_STATE_CONFLICT", "Call request conflicts with the current state.", false)
	case errors.Is(err, callsvc.ErrNotFound):
		writeError(w, http.StatusNotFound, "CALL_NOT_FOUND", "Call resource was not found.", false)
	case errors.Is(err, callsvc.ErrGatewayUnavailable):
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UNAVAILABLE", "The gateway is not available for calls.", true)
	case errors.Is(err, callsvc.ErrNoSubscribers):
		writeError(w, http.StatusServiceUnavailable, "NO_INCOMING_SUBSCRIBERS", "No client receives incoming calls on this node.", false)
	case errors.Is(err, callsvc.ErrSIMUnavailable):
		writeError(w, http.StatusConflict, "SIM_MAPPING_CHANGED", "The selected SIM mapping is unavailable or changed.", false)
	case errors.Is(err, callsvc.ErrCallIDConflict):
		writeError(w, http.StatusConflict, "CALL_ID_CONFLICT", "This call_id was already used for another call.", false)
	case errors.Is(err, callsvc.ErrBusy):
		writeError(w, http.StatusConflict, "GATEWAY_BUSY", "The gateway already has a call in progress.", false)
	default:
		writeDBUnavailable(w)
	}
}
