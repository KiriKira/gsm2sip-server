-- Latest radio observation reported by the gateway for each SIM. These columns
-- are display telemetry only: they never activate, route or verify a SIM.
ALTER TABLE sim_bindings
    ADD COLUMN signal_level SMALLINT CHECK (signal_level BETWEEN 0 AND 4),
    ADD COLUMN network_type TEXT CHECK (network_type ~ '^[A-Za-z0-9_+ -]{1,16}$'),
    ADD COLUMN network_operator TEXT CHECK (length(network_operator) BETWEEN 1 AND 64),
    ADD COLUMN roaming BOOLEAN,
    ADD COLUMN radio_updated_at TIMESTAMPTZ;
