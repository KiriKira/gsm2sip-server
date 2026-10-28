-- Reception v2: a client's lasting choice to receive incoming calls and SMS
-- wake-ups on this node. A device with a row at protocol_version 2 is offered
-- calls while incoming_enabled, without refreshing a capability or a sleep
-- grant; devices without a row keep the rtc_control_capabilities rules.
CREATE TABLE device_reception (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    protocol_version INT NOT NULL CHECK (protocol_version IN (1,2)),
    incoming_enabled BOOLEAN NOT NULL,
    sms_enabled BOOLEAN NOT NULL,
    revision BIGINT NOT NULL CHECK (revision>=1),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- generation counts endpoint replacements, so a late answer about an old
-- endpoint never changes the new one. failures/next_attempt_at keep pacing
-- SMS (and wire 1) wake-ups; call wake-ups count their own failures.
ALTER TABLE push_endpoints ADD COLUMN generation BIGINT NOT NULL DEFAULT 1;
ALTER TABLE push_endpoints ADD COLUMN call_failures INT NOT NULL DEFAULT 0;

-- Global decline: a host's "decline" ends the whole call on every device.
-- A gateway that can decide it declares global_decline; a client's request
-- waits in rtc_control_calls until the gateway's durable winner/decline
-- ledger decides and the cellular call really ends.
ALTER TABLE rtc_control_capabilities ADD COLUMN global_decline BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE rtc_control_calls ADD COLUMN decline_requested_by UUID REFERENCES devices(id) ON DELETE SET NULL;
ALTER TABLE rtc_control_calls ADD COLUMN decline_requested_at TIMESTAMPTZ;
