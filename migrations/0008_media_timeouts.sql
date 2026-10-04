-- An unreachable client must not leave a cellular leg billing indefinitely.
-- PJSIP terminates a channel after sustained lack of inbound media; holding a
-- call has a separate finite window. Both legs retain mandatory SDES-SRTP.
ALTER TABLE ps_endpoints
    ADD COLUMN rtp_timeout INTEGER NOT NULL DEFAULT 30 CHECK (rtp_timeout BETWEEN 15 AND 120),
    ADD COLUMN rtp_timeout_hold INTEGER NOT NULL DEFAULT 60 CHECK (rtp_timeout_hold BETWEEN 30 AND 180);
