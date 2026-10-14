-- Home country of each SIM (ISO 3166-1 alpha-2) as reported with its radio
-- telemetry. Clients use it to read national-format numbers on that SIM as
-- the same contact as their international form; it never routes a SIM.
ALTER TABLE sim_bindings
    ADD COLUMN country_iso TEXT CHECK (country_iso ~ '^[A-Z]{2}$');
