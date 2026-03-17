-- Unroll store queries.

-- name: InsertUnroll :exec
INSERT INTO unrolls (
    vtxo_outpoint_hash, vtxo_outpoint_index, status, current_level,
    leaf_confirm_height, error_msg, retry_count, last_broadcast_height,
    current_fee_rate, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: UpdateUnroll :exec
UPDATE unrolls
SET status = $3,
    current_level = $4,
    leaf_confirm_height = $5,
    error_msg = $6,
    retry_count = $7,
    last_broadcast_height = $8,
    current_fee_rate = $9,
    updated_at = $10
WHERE vtxo_outpoint_hash = $1
  AND vtxo_outpoint_index = $2;

-- name: GetUnroll :one
SELECT * FROM unrolls
WHERE vtxo_outpoint_hash = $1
  AND vtxo_outpoint_index = $2;

-- name: ListActiveUnrolls :many
-- Active unrolls are those not in terminal states:
-- complete (3) or failed (4).
SELECT * FROM unrolls
WHERE status NOT IN (3, 4)
ORDER BY created_at ASC;

-- name: DeleteUnroll :exec
DELETE FROM unrolls
WHERE vtxo_outpoint_hash = $1
  AND vtxo_outpoint_index = $2;
