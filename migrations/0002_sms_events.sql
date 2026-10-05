CREATE TABLE idempotency_records (
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload_hash BYTEA NOT NULL,
    response_status SMALLINT NOT NULL,
    response_body JSONB NOT NULL,
    resource_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, device_id, operation, idempotency_key)
);

CREATE TABLE messages (
    id UUID PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE CASCADE,
    sim_id UUID REFERENCES sim_bindings(sim_id) ON DELETE SET NULL,
    mapping_revision BIGINT,
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    from_address TEXT,
    to_address TEXT,
    body TEXT NOT NULL CHECK (octet_length(body) <= 16384),
    status TEXT NOT NULL CHECK (status IN ('received', 'queued', 'accepted_by_gateway', 'dispatching', 'submitted', 'delivered', 'failed', 'expired', 'unknown')),
    part_count INTEGER CHECK (part_count IS NULL OR part_count BETWEEN 1 AND 255),
    expires_at TIMESTAMPTZ,
    source_payload_hash BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_owner_created_idx ON messages(owner_id, created_at DESC, id DESC);
CREATE INDEX messages_gateway_created_idx ON messages(gateway_id, created_at DESC, id DESC);
CREATE INDEX messages_sim_created_idx ON messages(sim_id, created_at DESC, id DESC);

CREATE TABLE commands (
    id UUID PRIMARY KEY,
    message_id UUID NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE CASCADE,
    sim_id UUID NOT NULL REFERENCES sim_bindings(sim_id) ON DELETE RESTRICT,
    mapping_revision BIGINT NOT NULL,
    to_address TEXT NOT NULL,
    body TEXT NOT NULL CHECK (octet_length(body) <= 16384),
    status TEXT NOT NULL CHECK (status IN ('queued', 'accepted_by_gateway', 'dispatching', 'submitted', 'delivered', 'failed', 'expired', 'unknown')),
    expires_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX commands_claim_idx ON commands(gateway_id, status, created_at)
    WHERE status IN ('queued', 'accepted_by_gateway', 'dispatching');

CREATE TABLE message_parts (
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_index SMALLINT NOT NULL CHECK (part_index >= 0),
    state TEXT NOT NULL CHECK (state IN ('dispatching', 'submitted', 'delivered', 'failed', 'expired', 'unknown')),
    result_code INTEGER,
    error TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, part_index)
);

CREATE TABLE gateway_events (
    ack_cursor BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    gateway_id UUID NOT NULL REFERENCES gateways(device_id) ON DELETE CASCADE,
    event_id UUID NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    payload_hash BYTEA NOT NULL,
    event_json JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (gateway_id, event_id),
    UNIQUE (gateway_id, sequence)
);

CREATE TABLE owner_event_cursors (
	owner_id UUID PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
	last_cursor BIGINT NOT NULL DEFAULT 0 CHECK (last_cursor >= 0)
);

CREATE TABLE server_events (
	cursor BIGINT NOT NULL,
	id UUID NOT NULL UNIQUE,
	owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    event_json JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (owner_id,cursor)
);

CREATE TABLE outbox (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES server_events(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX outbox_pending_idx ON outbox(id) WHERE processed_at IS NULL;

CREATE TABLE sms_rate_ledger (
    sim_id UUID NOT NULL REFERENCES sim_bindings(sim_id) ON DELETE CASCADE,
    message_id UUID NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (sim_id, message_id)
);
CREATE INDEX sms_rate_ledger_recent_idx ON sms_rate_ledger(sim_id, created_at DESC);

CREATE TABLE audit_log (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    actor_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    resource_id UUID,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
