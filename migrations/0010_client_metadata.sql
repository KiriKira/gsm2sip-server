ALTER TABLE devices
    ADD COLUMN platform TEXT NOT NULL DEFAULT 'unknown'
    CHECK (platform ~ '^[a-z][a-z0-9_-]{0,31}$');
