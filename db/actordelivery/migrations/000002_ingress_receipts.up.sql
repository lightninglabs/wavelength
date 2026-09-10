-- Network-event consumption survives deletion of the consumer's inbox row.
-- A receipt commits in the same transaction as that first durable handoff.
CREATE TABLE ingress_receipts (
    id TEXT PRIMARY KEY,
    payload_hash BLOB NOT NULL,
    mailbox_id TEXT NOT NULL,
    consumed_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX ingress_receipts_expiry ON ingress_receipts(expires_at, id);
