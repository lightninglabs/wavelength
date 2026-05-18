-- name: InsertWalletUTXOLog :exec
-- Crash-replay safe: duplicate (outpoint, event) inserts from
-- retried ledger delivery are silently ignored so the audit log stays
-- at-most-once per outpoint+event.
INSERT INTO wallet_utxo_log (
    outpoint_hash, outpoint_index, amount_sat,
    event, block_height, classified_as, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (outpoint_hash, outpoint_index, event) DO NOTHING;

-- name: ListWalletUTXOLog :many
SELECT entry_id, outpoint_hash, outpoint_index, amount_sat,
       event, block_height, classified_as, created_at
FROM wallet_utxo_log
ORDER BY created_at DESC, entry_id DESC
LIMIT $1 OFFSET $2;

-- name: ListWalletUTXOLogByBlock :many
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
