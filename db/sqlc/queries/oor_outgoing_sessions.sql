-- name: UpsertOOROutgoingSession :exec
INSERT INTO oor_outgoing_sessions (
    session_id, snapshot_version, phase, snapshot_blob, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (session_id) DO UPDATE SET
    snapshot_version = $2,
    phase = $3,
    snapshot_blob = $4,
    updated_at = $6;

-- name: GetOOROutgoingSession :one
SELECT session_id, snapshot_version, phase, snapshot_blob, created_at, updated_at
FROM oor_outgoing_sessions
WHERE session_id = $1;
