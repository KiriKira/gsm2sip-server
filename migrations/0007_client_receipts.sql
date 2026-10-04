-- Receipt means the client reports a completed local SQLite transaction.
-- It does not mean a notification was shown or the human read the SMS.
-- Receipts never cause message/event deletion.
CREATE TABLE client_event_receipts (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    owner_id UUID NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    durable_cursor BIGINT NOT NULL CHECK (durable_cursor>=0),
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(device_id,owner_id) REFERENCES devices(id,owner_id) ON DELETE CASCADE
);
