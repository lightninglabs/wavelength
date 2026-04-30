-- Indexer VTXO event log.
--
-- This table stores a global, monotonic VTXO event feed that can be filtered
-- by recipient script. It is used by the indexer to provide durable
-- ListVTXOEventsByScripts pagination across restarts.
--
-- NOTE: This schema is written to be compatible with both sqlite and postgres.

CREATE TABLE IF NOT EXISTS indexer_vtxo_events (
    -- event_id is the global monotonic event cursor.
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- pk_script is the script this event is scoped to.
    pk_script BLOB NOT NULL,

    -- event_type is one of:
    --   - created
    --   - status_changed
    --   - terminated
    event_type TEXT NOT NULL,

    -- outpoint_hash + outpoint_index identify the affected VTXO.
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- status is the resulting VTXO status after the transition.
    status TEXT NOT NULL,

    -- created_at is the unix nano timestamp when the event row was written.
    created_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_indexer_vtxo_events_script_event
    ON indexer_vtxo_events(pk_script, event_id);
