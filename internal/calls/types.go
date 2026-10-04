package calls

import (
	"errors"
	"time"
)

var (
	ErrNotConfigured      = errors.New("calling is not configured")
	ErrNotReady           = errors.New("ARI call coordinator is not ready")
	ErrInvalid            = errors.New("invalid call request")
	ErrForbidden          = errors.New("call resource is not available to this client")
	ErrConflict           = errors.New("call state conflicts with this request")
	ErrNotFound           = errors.New("call resource not found")
	ErrGatewayUnavailable = errors.New("gateway is not available for calls")
	ErrSIMUnavailable     = errors.New("SIM mapping is unavailable or changed")
	ErrBusy               = errors.New("gateway already has a call in progress")
)

type Config struct {
	ARIURL         string
	ARIUsername    string
	ARIPassword    string
	ARIApplication string
	SIPRealm       string
	TokenKey       []byte
}

type CreateIntentRequest struct {
	GatewayID       string `json:"gateway_id"`
	SIMID           string `json:"sim_id"`
	MappingRevision int64  `json:"mapping_revision"`
	To              string `json:"to"`
}

type Intent struct {
	IntentID  string    `json:"intent_id"`
	CallID    string    `json:"call_id"`
	SIPURI    string    `json:"sip_uri"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Call struct {
	CallID          string     `json:"call_id"`
	GatewayID       string     `json:"gateway_id"`
	ClientID        string     `json:"client_id"`
	SIMID           string     `json:"sim_id"`
	MappingRevision int64      `json:"mapping_revision"`
	Direction       string     `json:"direction"`
	State           string     `json:"state"`
	StateRevision   int64      `json:"state_revision"`
	From            *string    `json:"from,omitempty"`
	To              string     `json:"to"`
	Reason          *string    `json:"reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	WakeNonce       *string    `json:"wake_nonce,omitempty"`
}

type ListResult struct {
	Items          []Call  `json:"items"`
	NextCursor     *string `json:"next_cursor"`
	ResyncRequired bool    `json:"resync_required"`
}

type ClientReadyRequest struct {
	CallID    string `json:"call_id"`
	WakeNonce string `json:"wake_nonce"`
}
