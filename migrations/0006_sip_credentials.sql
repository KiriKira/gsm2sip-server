-- Only the current SIP bootstrap response is retained, encrypted with the
-- deployment key. Asterisk sees a digest; HTTPS never returns the secret on GET.
CREATE TABLE device_sip_credentials (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    idempotency_hash BYTEA NOT NULL CHECK (octet_length(idempotency_hash)=32),
    response_ciphertext BYTEA NOT NULL,
    replay_expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE sip_credential_used_keys (
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    key_hash BYTEA NOT NULL CHECK (octet_length(key_hash)=32),
    PRIMARY KEY(device_id,key_hash)
);
