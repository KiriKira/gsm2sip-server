-- Experimental WebRTC audio adapter. SIP continues to own business call state,
-- gateway occupancy and incoming winner selection; this table never dials.
CREATE TABLE rtc_media_capabilities (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version = 1),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE rtc_media_sessions (
    call_id UUID NOT NULL REFERENCES call_sessions(call_id) ON DELETE CASCADE,
    client_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    leg_id TEXT NOT NULL UNIQUE,
    attempt_id TEXT NOT NULL,
    cellular_active BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (call_id, client_device_id)
);
