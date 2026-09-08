-- Record each keyed outgoing dispatch separately from the mutable session row.
-- The session row can later belong to an incoming self-transfer lifecycle, but
-- this record remains the canonical identity used by retry reconciliation.
CREATE TABLE oor_dispatch_attempts (
    idempotency_key TEXT PRIMARY KEY,
    session_id BLOB NOT NULL UNIQUE,
    request_data BLOB,
    created_at BIGINT NOT NULL
);

-- Existing session snapshots do not expose a canonical request to SQL. Retain
-- one conservative key-to-session binding so an upgrade cannot turn ambiguous
-- legacy work into a new send. New attempts always store request_data before
-- dispatch. When old failed rows reused one key, prefer a non-failed row and
-- otherwise keep the oldest binding.
INSERT INTO oor_dispatch_attempts (
    idempotency_key, session_id, request_data, created_at
)
SELECT idempotency_key, session_id, NULL, created_at
FROM (
    SELECT
        idempotency_key,
        session_id,
        created_at,
        ROW_NUMBER() OVER (
            PARTITION BY idempotency_key
            ORDER BY
                CASE WHEN status != 2 THEN 0 ELSE 1 END,
                created_at ASC,
                session_id ASC
        ) AS binding_rank
    FROM oor_session_registry
    WHERE idempotency_key IS NOT NULL
) AS legacy_bindings
WHERE binding_rank = 1;
