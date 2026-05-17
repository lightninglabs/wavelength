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
