CREATE TABLE refresh_recoveries (
    session_id UUID PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    old_refresh_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(old_refresh_hash) = 32),
    idempotency_key_hash BYTEA NOT NULL CHECK (octet_length(idempotency_key_hash) = 32),
    new_refresh_hash BYTEA NOT NULL CHECK (octet_length(new_refresh_hash) = 32),
    response_ciphertext BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_recoveries_expiry_idx ON refresh_recoveries(expires_at);
