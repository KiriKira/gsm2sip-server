-- One join code pairs both phones of an owner: each role can redeem it once.
-- Kept apart from pairing_codes so an older server never finds these codes
-- and simply rejects them.
CREATE TABLE join_codes (
    id UUID PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    code_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    gateway_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    client_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    gateway_redeemed_at TIMESTAMPTZ,
    client_redeemed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX join_codes_expiry_idx ON join_codes(expires_at);

-- FCM registration of a client device. The pushed cursors record the newest
-- SMS and call events already announced, so one push covers every event that
-- arrived since the previous one.
CREATE TABLE push_tokens (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    token TEXT NOT NULL CHECK (length(token) BETWEEN 1 AND 4096),
    pushed_event_cursor BIGINT NOT NULL DEFAULT 0,
    pushed_call_cursor BIGINT NOT NULL DEFAULT 0,
    failures INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
