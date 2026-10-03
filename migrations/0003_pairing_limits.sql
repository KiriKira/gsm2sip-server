CREATE TABLE pairing_rate_limits (
    remote_ip INET PRIMARY KEY,
    minute_start TIMESTAMPTZ NOT NULL,
    minute_attempts INTEGER NOT NULL CHECK (minute_attempts >= 0),
    hour_start TIMESTAMPTZ NOT NULL,
    hour_attempts INTEGER NOT NULL CHECK (hour_attempts >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
