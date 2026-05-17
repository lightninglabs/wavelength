-- name: InsertWalletEffect :exec
INSERT INTO wallet_effects (
    id, effect_type, status, idempotency_key, outpoint_hash,
    outpoint_index, txid, amount_sat, fee_sat, block_height,
    classification, attempts, max_attempts, next_attempt_at,
    created_at, updated_at
) VALUES (
    $1, $2, 'pending', $3, $4,
    $5, $6, $7, $8, $9,
    $10, 0, $11, $12,
    $13, $13
) ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ListDueWalletEffectIDs :many
SELECT id
FROM wallet_effects
WHERE status = 'pending'
  AND next_attempt_at <= $1
ORDER BY created_at, id
LIMIT $2;

-- name: ClaimWalletEffect :one
UPDATE wallet_effects
SET status = 'claimed',
    claim_owner = $2,
    claim_token = $3,
    claim_until = $4,
    attempts = attempts + 1,
    updated_at = $5
WHERE id = $1
  AND status = 'pending'
  AND next_attempt_at <= $5
  AND attempts < max_attempts
RETURNING id, effect_type, status, idempotency_key, outpoint_hash,
    outpoint_index, txid, amount_sat, fee_sat, block_height,
    classification, attempts, max_attempts, next_attempt_at,
    claim_owner, claim_token, claim_until, last_error, created_at,
    updated_at, done_at;

-- name: MarkWalletEffectDone :exec
UPDATE wallet_effects
SET status = 'done',
    done_at = $3,
    updated_at = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = NULL
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseWalletEffectForRetry :exec
UPDATE wallet_effects
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

-- name: ReleaseExpiredWalletEffectClaims :exec
UPDATE wallet_effects
SET status = 'pending',
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $2
WHERE status = 'claimed'
  AND claim_until <= $1;

