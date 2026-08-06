-- OOR session registry control-plane queries.

-- name: UpsertOORSessionRegistry :exec
INSERT INTO oor_session_registry (
    session_id, actor_id, direction, phase, idempotency_key, status,
    last_error, snapshot_data, snapshot_version, created_at, updated_at,
    flow_version, outgoing_snapshot_data, outgoing_snapshot_version,
    outgoing_status
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
    sqlc.narg(outgoing_snapshot_data),
    sqlc.narg(outgoing_snapshot_version),
    sqlc.narg(outgoing_status)
)
ON CONFLICT (session_id) DO UPDATE SET
    actor_id = EXCLUDED.actor_id,
    direction = EXCLUDED.direction,
    phase = EXCLUDED.phase,
    -- A sender can receive its own session without an outgoing dispatch key.
    -- The first durable binding is immutable: later same-session writes may
    -- repeat it, but can neither erase it nor replace it with another key.
    idempotency_key = COALESCE(
        oor_session_registry.idempotency_key,
        EXCLUDED.idempotency_key
    ),
    status = EXCLUDED.status,
    last_error = EXCLUDED.last_error,
    snapshot_data = EXCLUDED.snapshot_data,
    snapshot_version = EXCLUDED.snapshot_version,
    -- Recipient proof is the first artifact-rich outgoing snapshot. Later
    -- terminal outgoing snapshots intentionally omit the Ark PSBT, while an
    -- incoming lifecycle takes ownership of the shared current snapshot.
    outgoing_snapshot_data = COALESCE(
        oor_session_registry.outgoing_snapshot_data,
        EXCLUDED.outgoing_snapshot_data
    ),
    outgoing_snapshot_version = COALESCE(
        oor_session_registry.outgoing_snapshot_version,
        EXCLUDED.outgoing_snapshot_version
    ),
    outgoing_status = COALESCE(
        EXCLUDED.outgoing_status,
        oor_session_registry.outgoing_status
    ),
    updated_at = EXCLUDED.updated_at
;

-- name: GetOORSessionRegistry :one
SELECT * FROM oor_session_registry
WHERE session_id = $1
;

-- name: LookupActiveOORSessionRegistryByIdempotencyKey :one
-- Status 2 = Failed (anchored to Go iota in
-- db/oor_session_registry_store.go OORSessionStatus). A failed outgoing
-- session releases its key, while an incoming failure for a successfully sent
-- session keeps deduplicating against the retained outgoing status.
SELECT * FROM oor_session_registry
WHERE idempotency_key = $1
  AND COALESCE(outgoing_status, status) != 2
;

-- name: ListNonTerminalOORSessionRegistry :many
-- Status 1 = Completed, 2 = Failed (anchored to Go iota in
-- db/oor_session_registry_store.go OORSessionStatus).
SELECT * FROM oor_session_registry
WHERE status NOT IN (1, 2)
ORDER BY created_at ASC
;

-- name: ListAllOORSessionRegistry :many
SELECT * FROM oor_session_registry
ORDER BY created_at ASC
;
