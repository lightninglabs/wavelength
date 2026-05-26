-- vHTLC recovery job queries.

-- name: InsertVHTLCRecoveryJob :exec
INSERT INTO vhtlc_recovery_jobs (
    id, request_id, swap_id, direction, action, state, vtxo_txid,
    vtxo_vout, vtxo_amount_sat, sender_pubkey, receiver_pubkey,
    server_pubkey, refund_locktime, unilateral_claim_delay,
    unilateral_refund_delay, unilateral_refund_without_receiver_delay,
    preimage_hash, signer_key_family, signer_key_index, destination_script,
    max_fee_rate_sat_per_kw, unroll_target_outpoint_hash,
    unroll_target_outpoint_index, exit_policy_kind, created_at, updated_at,
    armed_at
) VALUES (
    $1, $2, $3, $4, $5, 'armed', $6, $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $24, $24
);

-- name: GetVHTLCRecoveryJob :one
SELECT * FROM vhtlc_recovery_jobs
WHERE id = $1;

-- name: GetVHTLCRecoveryJobByRequestID :one
SELECT * FROM vhtlc_recovery_jobs
WHERE request_id = $1;

-- name: GetVHTLCRecoveryJobBySwapAction :one
SELECT * FROM vhtlc_recovery_jobs
WHERE swap_id = $1
  AND action = $2;

-- name: ListNonTerminalVHTLCRecoveryJobs :many
SELECT * FROM vhtlc_recovery_jobs
WHERE state NOT IN ('completed', 'cancelled', 'failed')
ORDER BY updated_at ASC, created_at ASC;

-- name: ListVHTLCRecoveryJobs :many
SELECT * FROM vhtlc_recovery_jobs
ORDER BY updated_at ASC, created_at ASC;

-- name: EscalateVHTLCRecoveryJob :execrows
UPDATE vhtlc_recovery_jobs
SET state = CASE
        WHEN state = 'armed' THEN 'unroll_started'
        ELSE state
    END,
    updated_at = $2,
    escalated_at = COALESCE(escalated_at, $2),
    last_error = NULL
WHERE id = $1
  AND state NOT IN ('completed', 'cancelled', 'failed');

-- Intermediate states waiting_for_target and building_exit_spend are written
-- by the execution-layer PR. This storage slice accepts them as source states
-- so replay can resume from each durable pipeline boundary once those writes
-- exist.

-- name: MarkVHTLCRecoveryTargetDetected :exec
UPDATE vhtlc_recovery_jobs
SET state = 'waiting_for_csv',
    updated_at = $4,
    target_detected_at = COALESCE(target_detected_at, $4),
    unroll_target_outpoint_hash = $2,
    unroll_target_outpoint_index = $3
WHERE id = $1
  AND state IN ('unroll_started', 'waiting_for_target');

-- name: MarkVHTLCRecoveryExitTxBuilt :exec
UPDATE vhtlc_recovery_jobs
SET state = 'exit_spend_built',
    updated_at = $4,
    exit_tx_built_at = COALESCE(exit_tx_built_at, $4),
    exit_tx = $2,
    exit_txid = $3
WHERE id = $1
  AND state IN ('building_exit_spend', 'waiting_for_csv');

-- name: MarkVHTLCRecoveryExitTxSubmitting :exec
UPDATE vhtlc_recovery_jobs
SET state = 'submitting_exit_spend',
    updated_at = $2
WHERE id = $1
  AND state = 'exit_spend_built';

-- name: MarkVHTLCRecoveryExitTxBroadcast :exec
UPDATE vhtlc_recovery_jobs
SET state = 'exit_spend_pending_confirmation',
    updated_at = $2,
    exit_tx_broadcast_at = COALESCE(exit_tx_broadcast_at, $2)
WHERE id = $1
  AND state IN ('submitting_exit_spend', 'exit_spend_built');

-- name: CompleteVHTLCRecoveryJob :execrows
UPDATE vhtlc_recovery_jobs
SET state = 'completed',
    updated_at = $2,
    terminal_at = COALESCE(terminal_at, $2),
    last_error = NULL
WHERE id = $1
  AND state NOT IN ('completed', 'cancelled', 'failed');

-- name: CancelVHTLCRecoveryJob :execrows
UPDATE vhtlc_recovery_jobs
SET state = 'cancelled',
    updated_at = $4,
    terminal_at = COALESCE(terminal_at, $4),
    cancel_reason = $2,
    cooperative_txid = $3,
    last_error = NULL
WHERE id = $1
  AND state NOT IN ('completed', 'cancelled', 'failed');

-- name: FailVHTLCRecoveryJob :execrows
UPDATE vhtlc_recovery_jobs
SET state = 'failed',
    updated_at = $3,
    terminal_at = COALESCE(terminal_at, $3),
    last_error = $2
WHERE id = $1
  AND state NOT IN ('completed', 'cancelled', 'failed');
