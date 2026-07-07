-- OOR session registry control-plane queries.

-- name: UpsertOORSessionRegistry :exec
INSERT INTO oor_session_registry (
    session_id, actor_id, direction, phase, idempotency_key, status,
    last_error, snapshot_data, snapshot_version, created_at, updated_at,
    flow_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
ON CONFLICT (session_id) DO UPDATE SET
    actor_id = EXCLUDED.actor_id,
    direction = EXCLUDED.direction,
    phase = EXCLUDED.phase,
    idempotency_key = EXCLUDED.idempotency_key,
    status = EXCLUDED.status,
    last_error = EXCLUDED.last_error,
    snapshot_data = EXCLUDED.snapshot_data,
    snapshot_version = EXCLUDED.snapshot_version,
    updated_at = EXCLUDED.updated_at
;

-- name: GetOORSessionRegistry :one
SELECT * FROM oor_session_registry
WHERE session_id = $1
;

-- name: LookupActiveOORSessionRegistryByIdempotencyKey :one
-- Status 2 = Failed (anchored to Go iota in
-- db/oor_session_registry_store.go OORSessionStatus). Failed sessions never
-- dedup a keyed retry, so the lookup skips them: only a pending or completed
-- session answers for an idempotency key.
SELECT * FROM oor_session_registry
WHERE idempotency_key = $1 AND status != 2
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
