-- OOR recipient event log.
--
-- This table stores per-recipient notification cursors emitted after finalize
-- so clients can poll for incoming transfers without requiring a push channel.
-- Canonical package bytes are loaded via session join from oor_sessions and
-- oor_checkpoints.
--
-- NOTE: This schema is written to be compatible with both sqlite and postgres.

CREATE TABLE IF NOT EXISTS oor_recipient_events (
    -- recipient_pk_script is the destination script that owns this cursor
    -- sequence.
    recipient_pk_script BLOB NOT NULL,

    -- event_id is a per-recipient monotonic cursor assigned by the server.
    event_id BIGINT NOT NULL,

    -- session_db_id references the finalized OOR session integer PK.
    session_db_id INTEGER NOT NULL REFERENCES oor_sessions(id)
        ON DELETE CASCADE,

    -- output_index is the recipient output index in the Ark transaction.
    output_index INTEGER NOT NULL,

    -- value is the recipient output amount in satoshis.
    value BIGINT NOT NULL,

    -- created_at is the unix nano timestamp when the event row was written.
    created_at BIGINT NOT NULL,

    PRIMARY KEY(recipient_pk_script, event_id),

    -- Ensure idempotent inserts for the same recipient/session/output.
    UNIQUE (recipient_pk_script, session_db_id, output_index)
);

-- Speed up session-scoped scans/cascades.
CREATE INDEX IF NOT EXISTS idx_oor_recipient_events_session_db_id
    ON oor_recipient_events(session_db_id);
