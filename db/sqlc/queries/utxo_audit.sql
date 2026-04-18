-- name: InsertWalletUTXOLog :execrows
-- The UNIQUE(outpoint_hash, outpoint_index, event) constraint
-- plus ON CONFLICT DO NOTHING makes the per-block UTXO diff
-- loop crash-safe: a redelivered mailbox message or a
-- recomputed diff over the same block rewrites the same rows
-- without raising a constraint violation. :execrows returns
-- the rowcount so the diff loop can tell whether a write
-- landed (new UTXO change) or was silently deduped (replay).
INSERT INTO wallet_utxo_log (
    outpoint_hash, outpoint_index, amount_sat,
    event, block_height, classified_as, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING;

-- name: ListWalletUTXOLog :many
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at
FROM wallet_utxo_log
ORDER BY created_at DESC, entry_id DESC
LIMIT $1 OFFSET $2;

-- name: ListWalletUTXOLogByBlock :many
-- TODO(fees-03): add LIMIT/OFFSET when Admin RPC pagination lands.
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at
FROM wallet_utxo_log
WHERE block_height = $1
ORDER BY entry_id;

-- name: CountWalletUTXOLog :one
SELECT COUNT(*) FROM wallet_utxo_log;

-- name: ListWalletUTXOLogByClassification :many
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at
FROM wallet_utxo_log
WHERE classified_as = $1
ORDER BY created_at DESC, entry_id DESC
LIMIT $2 OFFSET $3;
