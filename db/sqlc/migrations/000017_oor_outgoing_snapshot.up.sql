-- Preserve the first artifact-bearing outgoing OOR snapshot separately from
-- the session row's active lifecycle snapshot. A sender can receive its own
-- OOR package under the same session id, so later terminal or incoming progress
-- must not erase the recipient proof needed to reconcile a lost response.
ALTER TABLE oor_session_registry
    ADD COLUMN outgoing_snapshot_data BLOB;

ALTER TABLE oor_session_registry
    ADD COLUMN outgoing_snapshot_version INTEGER;

ALTER TABLE oor_session_registry
    ADD COLUMN outgoing_status INTEGER;

-- These outgoing phases contain the Ark artifacts that keyed replay uses.
-- Later pending and terminal phases intentionally omit them, so they cannot be
-- proven after an upgrade and remain NULL here to make replay fail closed.
UPDATE oor_session_registry
SET outgoing_snapshot_data = snapshot_data,
    outgoing_snapshot_version = snapshot_version
WHERE direction = 1
  AND phase IN (
      'ark_sign_requested', 'submit_sent', 'cosigned', 'finalize_sent'
  );

-- Status is useful independently of recipient proof: it distinguishes a
-- failed outgoing transfer from a later incoming lifecycle on the shared row.
UPDATE oor_session_registry
SET outgoing_status = status
WHERE direction = 1;

-- An incoming failure must not release a successfully dispatched outgoing
-- key. Failed outgoing sessions still release their key for a fresh attempt.
DROP INDEX IF EXISTS idx_oor_session_registry_idempotency_key;

CREATE UNIQUE INDEX idx_oor_session_registry_idempotency_key
    ON oor_session_registry(idempotency_key)
    WHERE idempotency_key IS NOT NULL
      AND COALESCE(outgoing_status, status) != 2;
