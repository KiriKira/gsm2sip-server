-- Asterisk realtime PJSIP configuration. SIP passwords are never stored here;
-- password_digest is A1 = MD5(username:realm:password), prefixed with MD5:.
CREATE TABLE sip_endpoint_bindings (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    endpoint_id VARCHAR(80) NOT NULL UNIQUE,
    auth_username VARCHAR(80) NOT NULL UNIQUE,
    aor VARCHAR(80) NOT NULL UNIQUE,
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (endpoint_id ~ '^dev_[0-9a-f]{32}$')
);

CREATE TABLE ps_endpoints (
    id VARCHAR(80) PRIMARY KEY,
    transport TEXT,
    aors TEXT,
    auth TEXT,
    context TEXT,
    disallow TEXT,
    allow TEXT,
    direct_media TEXT,
    force_rport TEXT,
    rewrite_contact TEXT,
    rtp_symmetric TEXT,
    media_encryption TEXT,
    media_encryption_optimistic TEXT,
    dtmf_mode TEXT,
    identify_by TEXT,
    callerid TEXT
);

CREATE TABLE ps_auths (
    id VARCHAR(80) PRIMARY KEY,
    auth_type TEXT NOT NULL DEFAULT 'userpass',
    realm TEXT NOT NULL DEFAULT 'gsm2sip',
    username TEXT NOT NULL UNIQUE,
    password_digest VARCHAR(1024) NOT NULL,
    supported_algorithms_uas VARCHAR(1024) NOT NULL DEFAULT 'MD5',
    supported_algorithms_uac VARCHAR(1024) NOT NULL DEFAULT 'MD5',
    CHECK (password_digest ~ '^MD5:[0-9a-fA-F]{32}$')
);

CREATE TABLE ps_aors (
    id VARCHAR(80) PRIMARY KEY,
    max_contacts INTEGER NOT NULL DEFAULT 1,
    remove_existing TEXT NOT NULL DEFAULT 'yes',
    qualify_frequency INTEGER NOT NULL DEFAULT 60,
    CHECK (max_contacts = 1)
);

CREATE TABLE call_sessions (
    call_id UUID PRIMARY KEY,
    intent_id UUID UNIQUE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE RESTRICT,
    sim_id UUID NOT NULL REFERENCES sim_bindings(sim_id) ON DELETE RESTRICT,
    mapping_revision BIGINT NOT NULL CHECK (mapping_revision > 0),
    direction TEXT NOT NULL CHECK (direction IN ('outgoing', 'incoming')),
    from_address TEXT,
    to_address TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved', 'pending_wakeup', 'ringing', 'dialing', 'connecting', 'active', 'ended', 'unknown')),
    state_revision BIGINT NOT NULL DEFAULT 1 CHECK (state_revision > 0),
    reason TEXT,
    expires_at TIMESTAMPTZ,
    wake_nonce TEXT,
    gateway_endpoint_id VARCHAR(80) NOT NULL,
    client_endpoint_id VARCHAR(80) NOT NULL,
    gateway_channel_id TEXT,
    client_channel_id TEXT,
    ari_wrapper_channel_id TEXT,
    ari_wrapper_dial_started BOOLEAN NOT NULL DEFAULT false,
    bridge_id TEXT,
    gateway_sip_call_id TEXT,
    client_sip_call_id TEXT,
    answered_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    ari_observed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((state = 'ended') = (ended_at IS NOT NULL)),
    CHECK (wake_nonce IS NULL OR direction = 'incoming')
);
CREATE INDEX call_sessions_owner_client_idx ON call_sessions(owner_id, client_device_id, created_at DESC, call_id DESC);
CREATE INDEX call_sessions_gateway_state_idx ON call_sessions(gateway_id, state, updated_at DESC);
CREATE INDEX call_sessions_expiry_idx ON call_sessions(expires_at) WHERE state IN ('reserved', 'pending_wakeup', 'ringing');

CREATE TABLE call_intents (
    intent_id UUID PRIMARY KEY,
    call_id UUID NOT NULL UNIQUE REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE RESTRICT,
    sim_id UUID NOT NULL REFERENCES sim_bindings(sim_id) ON DELETE RESTRICT,
    mapping_revision BIGINT NOT NULL CHECK (mapping_revision > 0),
    to_address TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    state TEXT NOT NULL CHECK (state IN ('reserved', 'consumed', 'cancelled', 'expired')),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX call_intents_expiry_idx ON call_intents(expires_at) WHERE state = 'reserved';

CREATE TABLE call_intent_idempotency (
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    idempotency_key_hash BYTEA NOT NULL CHECK (octet_length(idempotency_key_hash) = 32),
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    intent_id UUID NOT NULL REFERENCES call_intents(intent_id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, client_device_id, idempotency_key_hash)
);

-- The row is retained while cellular work may have been dispatched. It is
-- removed only after a terminal state is confirmed or an unconsumed intent expires.
CREATE TABLE gateway_call_slots (
    gateway_id UUID PRIMARY KEY REFERENCES gateways(device_id) ON DELETE CASCADE,
    call_id UUID NOT NULL UNIQUE REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE call_events (
    cursor BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    call_id UUID NOT NULL REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    state_revision BIGINT NOT NULL,
    event_type TEXT NOT NULL,
    event_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (call_id, state_revision)
);
CREATE INDEX call_events_client_cursor_idx ON call_events(owner_id, client_device_id, cursor);

ALTER TABLE call_sessions
    ADD CONSTRAINT call_sessions_intent_fk FOREIGN KEY (intent_id) REFERENCES call_intents(intent_id) DEFERRABLE INITIALLY DEFERRED;
