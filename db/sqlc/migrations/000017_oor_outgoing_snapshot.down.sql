DROP INDEX IF EXISTS idx_oor_session_registry_idempotency_key;

-- Version 17 can retain a failed outgoing key after the row advances through
-- an incoming lifecycle. Version 16 filters only on current status, so clear
-- those already-released failed keys before recreating its unique index. A
-- newer retry row, if present, remains the sole owner of the key.
UPDATE oor_session_registry
SET idempotency_key = NULL
WHERE outgoing_status = 2;

CREATE UNIQUE INDEX idx_oor_session_registry_idempotency_key
    ON oor_session_registry(idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status != 2;

ALTER TABLE oor_session_registry
    DROP COLUMN outgoing_status;

ALTER TABLE oor_session_registry
    DROP COLUMN outgoing_snapshot_version;

ALTER TABLE oor_session_registry
    DROP COLUMN outgoing_snapshot_data;
