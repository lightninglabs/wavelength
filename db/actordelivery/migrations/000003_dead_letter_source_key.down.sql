-- Rebuild dead_letters back to a single-column primary key. This downgrade is
-- lossy when both mailbox and outbox dead letters exist for the same original
-- message ID: it keeps the oldest row for each ID.
CREATE TABLE IF NOT EXISTS dead_letters_v1 (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    message_type TEXT NOT NULL,
    payload BLOB NOT NULL,
    failure_reason TEXT NOT NULL,
    attempts INTEGER NOT NULL,
    created_at BIGINT NOT NULL
);

INSERT INTO dead_letters_v1 (
    id, source, actor_id, message_type, payload,
    failure_reason, attempts, created_at
)
SELECT
    d.id, d.source, d.actor_id, d.message_type, d.payload,
    d.failure_reason, d.attempts, d.created_at
FROM dead_letters d
WHERE NOT EXISTS (
    SELECT 1
    FROM dead_letters newer
    WHERE newer.id = d.id AND newer.created_at < d.created_at
);

DROP TABLE dead_letters;
ALTER TABLE dead_letters_v1 RENAME TO dead_letters;

CREATE INDEX IF NOT EXISTS idx_dead_letters_actor
    ON dead_letters(actor_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_dead_letters_source
    ON dead_letters(source, created_at DESC);
