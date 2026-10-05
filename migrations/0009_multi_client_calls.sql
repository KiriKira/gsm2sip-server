-- Incoming calls can be offered to several authorized client installations.
-- call_sessions remains the aggregate call and gateway lease; each SIP client
-- owns an independent nonce, ringing leg, and terminal result here.
ALTER TABLE call_events
    DROP CONSTRAINT call_events_call_id_state_revision_key;
ALTER TABLE call_events
    ADD CONSTRAINT call_events_call_client_revision_key
    UNIQUE (call_id, client_device_id, state_revision);

CREATE TABLE call_participants (
    call_id UUID NOT NULL REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    endpoint_id VARCHAR(80) NOT NULL,
    wake_nonce TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('candidate', 'pending_wakeup', 'ringing', 'connecting', 'accepted', 'ended')),
    state_revision BIGINT NOT NULL DEFAULT 1 CHECK (state_revision > 0),
    reason TEXT,
    client_channel_id TEXT,
    wrapper_channel_id TEXT,
    dial_started BOOLEAN NOT NULL DEFAULT false,
    accepted_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (call_id, client_device_id),
    UNIQUE (call_id, client_channel_id),
    UNIQUE (call_id, wrapper_channel_id),
    CHECK ((state = 'ended') = (ended_at IS NOT NULL)),
    CHECK (state <> 'accepted' OR accepted_at IS NOT NULL)
);
CREATE INDEX call_participants_client_idx
    ON call_participants(owner_id, client_device_id, created_at DESC, call_id DESC);
CREATE INDEX call_participants_live_idx
    ON call_participants(call_id, state) WHERE state <> 'ended';

-- Preserve incoming calls already in flight when the multi-client migration is
-- applied. Historical terminal rows remain visible to their original client.
INSERT INTO call_participants(
    call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state,state_revision,reason,
    client_channel_id,wrapper_channel_id,dial_started,accepted_at,ended_at,created_at,updated_at)
SELECT c.call_id,c.owner_id,c.client_device_id,c.client_endpoint_id,
       COALESCE(c.wake_nonce,'legacy-retired-'||c.call_id::text),
       CASE c.state
         WHEN 'pending_wakeup' THEN 'pending_wakeup'
         WHEN 'ringing' THEN 'ringing'
         WHEN 'connecting' THEN 'connecting'
         WHEN 'active' THEN 'accepted'
         ELSE 'ended'
       END,
       c.state_revision,c.reason,c.client_channel_id,c.ari_wrapper_channel_id,
       c.ari_wrapper_dial_started,
       CASE WHEN c.state='active' THEN COALESCE(c.answered_at,c.updated_at) END,
       CASE WHEN c.state NOT IN ('pending_wakeup','ringing','connecting','active') THEN COALESCE(c.ended_at,c.updated_at) END,
       c.created_at,c.updated_at
FROM call_sessions c
WHERE c.direction='incoming'
ON CONFLICT (call_id,client_device_id) DO NOTHING;

ALTER TABLE call_sessions
    ADD COLUMN incoming_winner_client_device_id UUID REFERENCES devices(id) ON DELETE SET NULL;

-- An already active legacy call has already selected its single SIP client.
UPDATE call_sessions SET incoming_winner_client_device_id=client_device_id
WHERE direction='incoming' AND state='active';
