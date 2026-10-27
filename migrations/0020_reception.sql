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
