-- name: InsertIndexerVTXOEvent :one
INSERT INTO indexer_vtxo_events (
    pk_script, event_type, outpoint_hash, outpoint_index, status, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING event_id;

-- name: ListIndexerVTXOEventsAfterByScriptsSqlite :many
SELECT event_id, pk_script, event_type, outpoint_hash, outpoint_index, status,
       created_at
FROM indexer_vtxo_events
WHERE pk_script IN (sqlc.slice('pk_scripts')/*SLICE:pk_scripts*/)
    AND event_id > $1
ORDER BY event_id ASC
LIMIT $2;

-- name: ListIndexerVTXOEventsAfterByScriptsPostgres :many
SELECT event_id, pk_script, event_type, outpoint_hash, outpoint_index, status,
       created_at
FROM indexer_vtxo_events
WHERE pk_script = ANY(@pk_scripts::bytea[])
    AND event_id > @after_event_id
ORDER BY event_id ASC
LIMIT @query_limit;
