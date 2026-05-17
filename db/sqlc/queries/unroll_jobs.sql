-- VTXO unroll job queries.

-- name: UpsertUnrollJob :exec
INSERT INTO unroll_jobs (
    target_outpoint_hash, target_outpoint_index, state, trigger,
    best_height, target_confirm_height, planner_state, deferred_checkpoints,
    sweep_tx, sweep_txid, sweep_confirm_height, sweep_attempts, fail_reason,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
ON CONFLICT (target_outpoint_hash, target_outpoint_index) DO UPDATE SET
    state = EXCLUDED.state,
    trigger = EXCLUDED.trigger,
    best_height = EXCLUDED.best_height,
    target_confirm_height = EXCLUDED.target_confirm_height,
    planner_state = EXCLUDED.planner_state,
    deferred_checkpoints = EXCLUDED.deferred_checkpoints,
    sweep_tx = EXCLUDED.sweep_tx,
    sweep_txid = EXCLUDED.sweep_txid,
    sweep_confirm_height = EXCLUDED.sweep_confirm_height,
    sweep_attempts = EXCLUDED.sweep_attempts,
    fail_reason = EXCLUDED.fail_reason,
    updated_at = EXCLUDED.updated_at
;

-- name: GetUnrollJob :one
SELECT * FROM unroll_jobs
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: ListNonTerminalUnrollJobs :many
SELECT * FROM unroll_jobs
WHERE state NOT IN ('completed', 'failed')
ORDER BY created_at ASC
;

-- name: MarkUnrollJobTerminal :exec
UPDATE unroll_jobs
SET state = $3,
    fail_reason = $4,
    updated_at = $5,
    sweep_txid = $6
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: DeleteUnrollTxProgressForJob :exec
DELETE FROM unroll_tx_progress
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: ListUnrollTxProgressForJob :many
SELECT * FROM unroll_tx_progress
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
ORDER BY role, txid
;

-- name: UpsertUnrollTxProgress :exec
INSERT INTO unroll_tx_progress (
    target_outpoint_hash, target_outpoint_index, txid, role, status,
    tx_bytes, confirm_height, last_error, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
ON CONFLICT (
    target_outpoint_hash, target_outpoint_index, txid, role
) DO UPDATE SET
    status = EXCLUDED.status,
    tx_bytes = EXCLUDED.tx_bytes,
    confirm_height = EXCLUDED.confirm_height,
    last_error = EXCLUDED.last_error,
    updated_at = EXCLUDED.updated_at
;

-- name: DeleteUnrollWatchesForJob :exec
DELETE FROM unroll_watches
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: ListUnrollWatchesForJob :many
SELECT * FROM unroll_watches
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
ORDER BY role, watch_id
;

-- name: UpsertUnrollWatch :exec
INSERT INTO unroll_watches (
    target_outpoint_hash, target_outpoint_index, watch_id, role, txid,
    spend_outpoint_hash, spend_outpoint_index, status, height_hint,
    confirmation_height, last_error, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (
    target_outpoint_hash, target_outpoint_index, watch_id
) DO UPDATE SET
    role = EXCLUDED.role,
    txid = EXCLUDED.txid,
    spend_outpoint_hash = EXCLUDED.spend_outpoint_hash,
    spend_outpoint_index = EXCLUDED.spend_outpoint_index,
    status = EXCLUDED.status,
    height_hint = EXCLUDED.height_hint,
    confirmation_height = EXCLUDED.confirmation_height,
    last_error = EXCLUDED.last_error,
    updated_at = EXCLUDED.updated_at
;
