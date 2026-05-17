-- name: SaveUnrollCheckpoint :exec
INSERT INTO unroll_checkpoints (
    actor_id, state_type, state_data, version, updated_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (actor_id) DO UPDATE SET
    state_type = excluded.state_type,
    state_data = excluded.state_data,
    version = excluded.version,
    updated_at = excluded.updated_at;

-- name: GetUnrollCheckpoint :one
SELECT actor_id, state_type, state_data, version, updated_at
FROM unroll_checkpoints
WHERE actor_id = $1;
