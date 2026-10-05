CREATE TABLE owners (
    id UUID PRIMARY KEY,
    display_name TEXT NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    id UUID PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('gateway', 'client')),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, owner_id)
);
CREATE INDEX devices_owner_role_idx ON devices(owner_id, role) WHERE state = 'active';

CREATE TABLE pairing_codes (
    id UUID PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('gateway', 'client')),
    code_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pairing_codes_expiry_idx ON pairing_codes(expires_at);

CREATE TABLE sessions (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    access_hash BYTEA NOT NULL UNIQUE,
    access_expires_at TIMESTAMPTZ NOT NULL,
    refresh_hash BYTEA NOT NULL UNIQUE,
    refresh_expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at TIMESTAMPTZ
);
CREATE INDEX sessions_device_idx ON sessions(device_id);

CREATE TABLE gateways (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    mapping_revision BIGINT NOT NULL DEFAULT 0 CHECK (mapping_revision >= 0),
    heartbeat_sequence BIGINT,
    heartbeat_hash BYTEA,
    heartbeat_result JSONB,
    last_seen_at TIMESTAMPTZ,
    protocol_version INTEGER,
    app_version TEXT,
    rooted BOOLEAN,
    sip_registered BOOLEAN,
    battery_percent SMALLINT CHECK (battery_percent BETWEEN 0 AND 100),
    charging BOOLEAN
);

CREATE TABLE sim_bindings (
    sim_id UUID PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE CASCADE,
    slot_index SMALLINT NOT NULL CHECK (slot_index BETWEEN 0 AND 1),
    label TEXT NOT NULL CHECK (length(label) BETWEEN 1 AND 64),
    carrier_name TEXT,
    phone_number TEXT,
    state TEXT NOT NULL CHECK (state IN ('pending_local_confirmation', 'active', 'unverified', 'removed')),
    identity_verified BOOLEAN NOT NULL DEFAULT false,
    mapping_revision BIGINT NOT NULL CHECK (mapping_revision > 0),
    service_state TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (gateway_id, owner_id) REFERENCES devices(id, owner_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX sim_bindings_live_slot_idx ON sim_bindings(gateway_id, slot_index)
    WHERE state IN ('pending_local_confirmation', 'active', 'unverified');
CREATE INDEX sim_bindings_owner_gateway_idx ON sim_bindings(owner_id, gateway_id);

CREATE TABLE sim_binding_operations (
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE CASCADE,
    operation_id UUID NOT NULL,
    proposal_hash BYTEA NOT NULL,
    proposal_result JSONB NOT NULL,
    confirmation_hash BYTEA,
    confirmation_result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (gateway_id, operation_id)
);
