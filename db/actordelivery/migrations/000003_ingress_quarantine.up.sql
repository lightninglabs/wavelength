-- Poison payloads remain recoverable after the transport advances. There is
-- deliberately no expiry column: an unresolved obligation must not age out.
CREATE TABLE ingress_quarantine (
    id TEXT PRIMARY KEY,
    lane TEXT NOT NULL,
    envelope BLOB NOT NULL,
    reason TEXT NOT NULL,
    attempts BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL
);
CREATE INDEX ingress_quarantine_lane ON ingress_quarantine(lane, created_at, id);
