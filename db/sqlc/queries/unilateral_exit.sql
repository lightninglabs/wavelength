-- Unilateral-exit job control-plane queries.

-- name: UpsertUnilateralExitJob :exec
INSERT INTO unilateral_exit_jobs (
    target_outpoint_hash, target_outpoint_index, actor_id, status, trigger,
    exit_policy_kind, exit_policy_ref, last_error, sweep_txid, created_at,
    updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
ON CONFLICT (target_outpoint_hash, target_outpoint_index) DO UPDATE SET
    actor_id = EXCLUDED.actor_id,
    status = EXCLUDED.status,
    trigger = EXCLUDED.trigger,
    exit_policy_kind = EXCLUDED.exit_policy_kind,
    exit_policy_ref = EXCLUDED.exit_policy_ref,
    last_error = EXCLUDED.last_error,
    sweep_txid = EXCLUDED.sweep_txid,
    updated_at = EXCLUDED.updated_at
;

-- name: GetUnilateralExitJob :one
SELECT * FROM unilateral_exit_jobs
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: ListNonTerminalUnilateralExitJobs :many
-- Status 4 = Completed, 5 = Failed, 7 = FailedRecoverable (anchored to Go
-- iota in db/unilateral_exit_store.go UnilateralExitJobStatus).
SELECT * FROM unilateral_exit_jobs
WHERE status NOT IN (4, 5, 7)
ORDER BY created_at ASC
;

-- name: MarkUnilateralExitJobTerminal :exec
UPDATE unilateral_exit_jobs
SET status = $3,
    last_error = $4,
    updated_at = $5,
    sweep_txid = $6
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- Exit funding address persistence queries (wavelength#893).

-- name: GetExitFundingAddress :one
SELECT * FROM exit_funding_addresses
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: InsertExitFundingAddress :exec
INSERT INTO exit_funding_addresses (
    target_outpoint_hash, target_outpoint_index, funding_address, created_at
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (target_outpoint_hash, target_outpoint_index) DO NOTHING
;
