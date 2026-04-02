-- Unilateral-exit job control-plane queries.

-- name: UpsertUnilateralExitJob :exec
INSERT INTO unilateral_exit_jobs (
    target_outpoint_hash, target_outpoint_index, actor_id, status, trigger,
    last_error, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
ON CONFLICT (target_outpoint_hash, target_outpoint_index) DO UPDATE SET
    actor_id = EXCLUDED.actor_id,
    status = EXCLUDED.status,
    trigger = EXCLUDED.trigger,
    last_error = EXCLUDED.last_error,
    updated_at = EXCLUDED.updated_at
;

-- name: GetUnilateralExitJob :one
SELECT * FROM unilateral_exit_jobs
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;

-- name: ListNonTerminalUnilateralExitJobs :many
-- Status 4 = Completed, 5 = Failed (anchored to Go iota in
-- db/unilateral_exit_store.go UnilateralExitJobStatus).
SELECT * FROM unilateral_exit_jobs
WHERE status NOT IN (4, 5)
ORDER BY created_at ASC
;

-- name: MarkUnilateralExitJobTerminal :exec
UPDATE unilateral_exit_jobs
SET status = $3,
    last_error = $4,
    updated_at = $5
WHERE target_outpoint_hash = $1
  AND target_outpoint_index = $2
;
