-- Rebuild dead_letters so mailbox and outbox rows can coexist under the same
-- propagated message ID. Dead-letter identity is `(source, id)`, not `id`
-- alone, because outbox IDs are intentionally reused as mailbox IDs for
-- receiver-side deduplication.
CREATE TABLE IF NOT EXISTS dead_letters_v2 (
    id TEXT NOT NULL,
    source TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    message_type TEXT NOT NULL,
    payload BLOB NOT NULL,
    failure_reason TEXT NOT NULL,
    attempts INTEGER NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (source, id)
);

INSERT INTO dead_letters_v2 (
    id, source, actor_id, message_type, payload,
    failure_reason, attempts, created_at
)
SELECT
    id, source, actor_id, message_type, payload,
    failure_reason, attempts, created_at
FROM dead_letters;

DROP TABLE dead_letters;
ALTER TABLE dead_letters_v2 RENAME TO dead_letters;

CREATE INDEX IF NOT EXISTS idx_dead_letters_actor
    ON dead_letters(actor_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_dead_letters_source
    ON dead_letters(source, created_at DESC);
