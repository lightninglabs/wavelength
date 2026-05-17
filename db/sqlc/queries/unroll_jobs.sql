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

-- name: InsertUnrollEffect :exec
INSERT INTO unroll_effects (
    id, target_outpoint_hash, target_outpoint_index, effect_type, txid,
    status, idempotency_key, attempts, max_attempts, next_attempt_at,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, 'pending', $6, 0, $7, $8, $9, $9
) ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ListDueUnrollEffectIDs :many
SELECT id
FROM unroll_effects
WHERE next_attempt_at <= $1
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $1)
  )
ORDER BY next_attempt_at, created_at, id
LIMIT $2;

-- name: ClaimUnrollEffect :one
UPDATE unroll_effects
SET status = 'claimed',
    claim_owner = $2,
    claim_token = $3,
    claim_until = $4,
    attempts = attempts + 1,
    updated_at = $5
WHERE id = $1
  AND next_attempt_at <= $5
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $5)
  )
RETURNING id, target_outpoint_hash, target_outpoint_index, effect_type, txid,
    status, idempotency_key, attempts, max_attempts, next_attempt_at,
    claim_owner, claim_token, claim_until, last_error, created_at,
    updated_at, done_at;

-- name: MarkUnrollEffectDone :exec
UPDATE unroll_effects
SET status = 'done',
    done_at = $3,
    updated_at = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = NULL
WHERE id = $1
  AND status IN ('pending', 'claimed')
  AND ($2 IS NULL OR claim_token = $2);

-- name: ReleaseUnrollEffectForRetry :exec
UPDATE unroll_effects
SET status = CASE
        WHEN attempts >= max_attempts THEN 'dead'
        ELSE 'pending'
    END,
    next_attempt_at = $3,
    updated_at = $4,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = $5
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseExpiredUnrollEffectClaims :exec
UPDATE unroll_effects
SET status = 'pending',
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $2
WHERE status = 'claimed'
  AND claim_until <= $1;
