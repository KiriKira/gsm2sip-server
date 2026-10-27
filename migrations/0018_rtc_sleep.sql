-- A client whose push endpoint works may let its call-control capability
-- sleep: until sleep_until it stays an incoming-call candidate without the
-- 90 s refresh, and the call reaches it as a push wake-up instead.
ALTER TABLE rtc_control_capabilities ADD COLUMN sleep_until TIMESTAMPTZ;
-- When this node last pushed a verification hint to a sleeping client.
ALTER TABLE push_endpoints ADD COLUMN sleep_checked_at TIMESTAMPTZ;
