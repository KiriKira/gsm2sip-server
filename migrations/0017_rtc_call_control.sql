-- SIP remains an explicitly selected compatibility transport. Both engines use
-- the same call history, participant identities and exclusive cellular slot.
ALTER TABLE call_sessions ADD COLUMN control_transport TEXT NOT NULL DEFAULT 'sip'
    CHECK (control_transport IN ('sip','rtc'));
CREATE TABLE rtc_control_capabilities (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version=1),
    enabled BOOLEAN NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE rtc_control_calls (
    call_id UUID PRIMARY KEY REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    end_requested BOOLEAN NOT NULL DEFAULT false,
    dispatched BOOLEAN NOT NULL DEFAULT false,
    candidate_client_id UUID REFERENCES devices(id) ON DELETE RESTRICT,
    candidate_until TIMESTAMPTZ,
    gateway_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    client_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE rtc_control_operations (
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    operation_id UUID NOT NULL,
    call_id UUID NOT NULL REFERENCES call_sessions(call_id) ON DELETE RESTRICT,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash)=32),
    result JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (device_id,operation_id)
);
CREATE INDEX rtc_control_operations_call_idx ON rtc_control_operations(call_id);
