-- SQLite does not support DROP COLUMN, so recreate the table.
-- For PostgreSQL, use ALTER TABLE ... DROP COLUMN.

CREATE TABLE indexer_vtxo_events_backup AS
SELECT event_id, pk_script, event_type, outpoint_hash, outpoint_index,
       status, created_at
FROM indexer_vtxo_events;

DROP TABLE indexer_vtxo_events;

ALTER TABLE indexer_vtxo_events_backup RENAME TO indexer_vtxo_events;

CREATE INDEX IF NOT EXISTS idx_indexer_vtxo_events_script_event
    ON indexer_vtxo_events(pk_script, event_id);
