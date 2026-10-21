-- Dormant RTC negotiation storage. No API or capability is enabled by this
-- migration. The future call coordinator MUST authorize engine, shared gateway
-- occupancy, SIM binding and participant/winner scope before creating a leg.
-- No rows are inserted by the current SIP/ARI code.
CREATE TABLE rtc_signal_legs (
    leg_id TEXT PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    checkpoint BYTEA NOT NULL CHECK (octet_length(checkpoint) <= 4194304),
    next_expiry TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX rtc_signal_legs_expiry ON rtc_signal_legs(next_expiry) WHERE next_expiry IS NOT NULL;
