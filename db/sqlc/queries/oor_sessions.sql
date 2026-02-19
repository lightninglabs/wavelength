-- name: UpsertOORSession :one
INSERT INTO oor_sessions (
    session_id, state, ark_psbt,
    created_at, updated_at, expires_at, finalized_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (session_id) DO UPDATE SET
    state = excluded.state,
    updated_at = excluded.updated_at,
    expires_at = excluded.expires_at,
    finalized_at = excluded.finalized_at
RETURNING id;

-- name: GetOORSession :one
SELECT id, session_id, state, ark_psbt,
       created_at, updated_at, expires_at, finalized_at
FROM oor_sessions
WHERE session_id = $1;

-- name: GetOORSessionByID :one
SELECT id, session_id, state, ark_psbt,
       created_at, updated_at, expires_at, finalized_at
FROM oor_sessions
WHERE id = $1;

-- name: ApplyFinalizeOORSession :execrows
UPDATE oor_sessions
SET state = 'awaiting_notify',
    updated_at = $2,
    finalized_at = $3
WHERE session_id = $1
  AND state = 'cosigned';

-- name: MarkOORSessionNotified :execrows
UPDATE oor_sessions
SET state = 'finalized',
    updated_at = $2
WHERE session_id = $1
  AND state = 'awaiting_notify';

-- name: ListActiveOORSessions :many
SELECT id, session_id, state, ark_psbt,
       created_at, updated_at, expires_at, finalized_at
FROM oor_sessions
WHERE state IN ('cosigned', 'awaiting_notify')
ORDER BY updated_at DESC;

-- name: DeleteOORCheckpoints :exec
DELETE FROM oor_checkpoints WHERE session_db_id = $1;

-- name: UpsertOORCheckpoint :exec
INSERT INTO oor_checkpoints (
    session_db_id, checkpoint_index, input_txid, input_vout, checkpoint_psbt
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (session_db_id, checkpoint_index) DO UPDATE SET
    checkpoint_psbt = excluded.checkpoint_psbt;

-- name: ListOORCheckpoints :many
SELECT session_db_id, checkpoint_index, input_txid, input_vout,
       checkpoint_psbt
FROM oor_checkpoints
WHERE session_db_id = $1
ORDER BY checkpoint_index ASC;
