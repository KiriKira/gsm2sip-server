package httpapi

import (
	"encoding/json"
	"time"
)

type Principal struct {
	DeviceID string
	OwnerID  string
	Role     string
	Name     string
}

type PairingClaimRequest struct {
	PairingCode string `json:"pairing_code"`
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform,omitempty"`
	// Role is required for a join code, which pairs either phone; a
	// pairing code fixes the role itself and ignores this field.
	Role string `json:"role,omitempty"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type SessionTokens struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type PairingClaimResponse struct {
	OwnerID  string `json:"owner_id"`
	DeviceID string `json:"device_id"`
	Role     string `json:"role"`
	SessionTokens
}

type HeartbeatRequest struct {
	Sequence        int64  `json:"sequence"`
	ProtocolVersion int    `json:"protocol_version"`
	AppVersion      string `json:"app_version"`
	Root            bool   `json:"root"`
	// Accepted and ignored: gateways before 0.4 still send their SIP state.
	SIPRegistered  bool       `json:"sip_registered"`
	BatteryPercent *int       `json:"battery_percent"`
	Charging       *bool      `json:"charging"`
	SIMStates      []SIMState `json:"sim_states"`
	// InstallID identifies the physical gateway install across independent
	// nodes; sent only after a heartbeat response lists HeartbeatExtensionMesh.
	InstallID *string `json:"install_id,omitempty"`
}

type SIMState struct {
	SIMID            string `json:"sim_id"`
	MappingRevision  int64  `json:"mapping_revision"`
	ServiceState     string `json:"service_state"`
	IdentityVerified bool   `json:"identity_verified"`
	// Radio is optional display telemetry. Gateways send it only after a
	// heartbeat response lists HeartbeatExtensionSIMRadio.
	Radio *SIMRadio `json:"radio,omitempty"`
}

// HeartbeatExtensionSIMRadio is advertised in heartbeat responses so a newer
// gateway never sends fields that an older server would reject as unknown.
const HeartbeatExtensionSIMRadio = "sim_radio"

// HeartbeatExtensionSIMCountry marks support for SIMRadio.CountryISO, which
// a gateway sends only when the heartbeat response lists it.
const HeartbeatExtensionSIMCountry = "sim_country"

// HeartbeatExtensionMesh marks support for multi-node operation: client-minted
// message IDs, gateway-proposed SIM IDs and the gateway install ID.
const HeartbeatExtensionMesh = "mesh"

// SupportedExtensions is advertised by heartbeats and GET /v1/capabilities.
var SupportedExtensions = []string{HeartbeatExtensionSIMRadio, HeartbeatExtensionSIMCountry, HeartbeatExtensionMesh}

type SIMRadio struct {
	SignalLevel     *int    `json:"signal_level"`
	NetworkType     *string `json:"network_type"`
	NetworkOperator *string `json:"network_operator"`
	Roaming         *bool   `json:"roaming"`
	// CountryISO is the SIM's home country, ISO 3166-1 alpha-2 in upper case.
	CountryISO *string `json:"country_iso"`
}

type HeartbeatResponse struct {
	ServerTime        time.Time `json:"server_time"`
	Online            bool      `json:"online"`
	StaleAfterSeconds int       `json:"stale_after_seconds"`
	MappingRevision   int64     `json:"mapping_revision"`
	InvalidatedSIMIDs []string  `json:"invalidated_sim_ids"`
	Extensions        []string  `json:"extensions"`
}

type SIMProposalRequest struct {
	OperationID string        `json:"operation_id"`
	Phase       string        `json:"phase"`
	Mappings    []SIMProposal `json:"mappings"`
}

type SIMProposal struct {
	SlotIndex       int     `json:"slot_index"`
	Label           string  `json:"label"`
	CarrierName     *string `json:"carrier_name"`
	PhoneNumber     *string `json:"phone_number"`
	ExistingSIMID   *string `json:"existing_sim_id"`
	SameSIMVerified bool    `json:"same_sim_verified"`
	// ProposedSIMID lets a multi-node gateway keep one SIM ID on every node.
	ProposedSIMID *string `json:"proposed_sim_id,omitempty"`
}

type SIMConfirmationRequest struct {
	OperationID   string            `json:"operation_id"`
	Phase         string            `json:"phase"`
	Confirmations []SIMConfirmation `json:"confirmations"`
}

type SIMConfirmation struct {
	SIMID     string `json:"sim_id"`
	SlotIndex int    `json:"slot_index"`
	Confirmed bool   `json:"confirmed"`
}

type SIMBindingResult struct {
	OperationID     string              `json:"operation_id"`
	Phase           string              `json:"phase"`
	MappingRevision int64               `json:"mapping_revision"`
	Mappings        []SIMBindingSummary `json:"mappings"`
}

type SIMBindingSummary struct {
	SIMID           string `json:"sim_id"`
	SlotIndex       int    `json:"slot_index"`
	State           string `json:"state"`
	MappingRevision int64  `json:"mapping_revision"`
}

type CreateMessageRequest struct {
	// MessageID is minted by the client when the server advertises the "mesh"
	// extension, so a retry through another node keeps one identity and the
	// gateway's ledger can refuse a second execution.
	MessageID       *string `json:"message_id,omitempty"`
	GatewayID       string  `json:"gateway_id"`
	SIMID           string  `json:"sim_id"`
	MappingRevision int64   `json:"mapping_revision"`
	To              string  `json:"to"`
	Text            string  `json:"text"`
	TTLSeconds      int     `json:"ttl_seconds"`
}

type MessageAccepted struct {
	MessageID string    `json:"message_id"`
	CommandID string    `json:"command_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type MessagePart struct {
	PartIndex  int     `json:"part_index"`
	State      string  `json:"state"`
	ResultCode *int    `json:"result_code"`
	Error      *string `json:"error"`
}

type MessageDetail struct {
	MessageID       string        `json:"message_id"`
	CommandID       *string       `json:"command_id"`
	GatewayID       string        `json:"gateway_id"`
	SIMID           *string       `json:"sim_id"`
	MappingRevision *int64        `json:"mapping_revision"`
	Direction       string        `json:"direction"`
	From            *string       `json:"from"`
	To              *string       `json:"to"`
	Text            string        `json:"text"`
	Status          string        `json:"status"`
	PartCount       *int          `json:"part_count"`
	CreatedAt       time.Time     `json:"created_at"`
	ExpiresAt       *time.Time    `json:"expires_at"`
	Parts           []MessagePart `json:"parts"`
}

type Command struct {
	CommandID       string    `json:"command_id"`
	MessageID       string    `json:"message_id"`
	GatewayID       string    `json:"gateway_id"`
	SIMID           string    `json:"sim_id"`
	MappingRevision int64     `json:"mapping_revision"`
	To              string    `json:"to"`
	Text            string    `json:"text"`
	ExpiresAt       time.Time `json:"expires_at"`
	PayloadSHA256   string    `json:"payload_sha256"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"-"`
}

type GatewayEvent struct {
	ProtocolVersion int            `json:"protocol_version"`
	EventID         string         `json:"event_id"`
	GatewayID       string         `json:"gateway_id"`
	Sequence        int64          `json:"sequence"`
	OccurredAt      time.Time      `json:"occurred_at"`
	Type            string         `json:"type"`
	SIMID           *string        `json:"sim_id"`
	MappingRevision *int64         `json:"mapping_revision"`
	SIMResolution   *string        `json:"sim_resolution,omitempty"`
	Payload         map[string]any `json:"payload"`
}

type EventAck struct {
	EventID   string `json:"event_id"`
	Cursor    string `json:"event_cursor"`
	Duplicate bool   `json:"duplicate"`
}

type EventEnvelope struct {
	Cursor     string          `json:"cursor"`
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Message    *MessageDetail  `json:"message"`
	RawMessage json.RawMessage `json:"-"`
}
