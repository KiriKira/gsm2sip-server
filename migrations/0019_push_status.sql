-- When this node last handed a wake-up for the device to its push endpoint
-- (the relay accepted it), and when the device last asked for a test push.
ALTER TABLE push_endpoints ADD COLUMN delivered_at TIMESTAMPTZ;
ALTER TABLE push_endpoints ADD COLUMN tested_at TIMESTAMPTZ;
