-- name: InsertAssetEngineKey :exec
-- InsertAssetEngineKey records a derived key. A known key inserts nothing.
INSERT INTO asset_engine_keys (pubkey, key_family, key_index)
VALUES ($1, $2, $3)
ON CONFLICT (pubkey) DO NOTHING;

-- name: GetAssetEngineKey :one
SELECT * FROM asset_engine_keys WHERE pubkey = $1;

-- name: UpsertAssetEngineScriptKey :exec
INSERT INTO asset_engine_script_keys (
    tweaked_key, raw_key, key_family, key_index, tweak, key_type
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tweaked_key) DO UPDATE SET
    raw_key = EXCLUDED.raw_key,
    key_family = EXCLUDED.key_family,
    key_index = EXCLUDED.key_index,
    tweak = EXCLUDED.tweak,
    key_type = EXCLUDED.key_type;

-- name: GetAssetEngineScriptKey :one
SELECT * FROM asset_engine_script_keys WHERE tweaked_key = $1;

-- name: UpsertAssetEngineProof :exec
-- UpsertAssetEngineProof stores a proof file. A known output keeps its spent
-- flag.
INSERT INTO asset_engine_proofs (
    asset_id, script_key, outpoint_hash, outpoint_index, proof_file, owned
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (asset_id, script_key, outpoint_hash, outpoint_index)
DO UPDATE SET
    proof_file = EXCLUDED.proof_file,
    owned = EXCLUDED.owned;

-- name: ListAssetEngineProofsByKey :many
SELECT * FROM asset_engine_proofs
WHERE asset_id = $1 AND script_key = $2
ORDER BY outpoint_hash, outpoint_index;

-- name: GetAssetEngineProof :one
SELECT * FROM asset_engine_proofs
WHERE asset_id = $1 AND script_key = $2 AND outpoint_hash = $3
    AND outpoint_index = $4;

-- name: ListOwnedAssetEngineProofs :many
-- ListOwnedAssetEngineProofs returns every output the wallet holds or held.
SELECT * FROM asset_engine_proofs
WHERE owned
ORDER BY outpoint_hash, outpoint_index, asset_id, script_key;

-- name: ListUnspentAssetEngineProofs :many
-- ListUnspentAssetEngineProofs returns the outputs the wallet holds.
SELECT * FROM asset_engine_proofs
WHERE owned AND NOT spent
ORDER BY outpoint_hash, outpoint_index, asset_id, script_key;

-- name: MarkAssetEngineProofSpent :execrows
UPDATE asset_engine_proofs SET spent = TRUE
WHERE asset_id = $1 AND script_key = $2 AND outpoint_hash = $3
    AND outpoint_index = $4;

-- name: DeleteAssetEngineProof :exec
DELETE FROM asset_engine_proofs
WHERE asset_id = $1 AND script_key = $2 AND outpoint_hash = $3
    AND outpoint_index = $4;

-- name: InsertAssetEngineTransfer :execrows
-- InsertAssetEngineTransfer records a transfer. A recorded anchor
-- transaction inserts nothing.
INSERT INTO asset_engine_transfers (
    anchor_txid, state, height_hint, block_hash, block_height, created_at,
    payload
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (anchor_txid) DO NOTHING;

-- name: UpdateAssetEngineTransfer :execrows
UPDATE asset_engine_transfers SET
    state = $2,
    block_hash = $3,
    block_height = $4,
    payload = $5
WHERE anchor_txid = $1;

-- name: GetAssetEngineTransfer :one
SELECT * FROM asset_engine_transfers WHERE anchor_txid = $1;

-- name: ListAssetEngineTransfers :many
SELECT * FROM asset_engine_transfers ORDER BY created_at, anchor_txid;

-- name: UpsertAssetJournalEntry :exec
INSERT INTO asset_journal (journal_key, value) VALUES ($1, $2)
ON CONFLICT (journal_key) DO UPDATE SET value = EXCLUDED.value;

-- name: GetAssetJournalEntry :one
SELECT value FROM asset_journal WHERE journal_key = $1;

-- name: InsertAssetEngineAddress :execrows
-- InsertAssetEngineAddress records an address. A recorded script key inserts
-- nothing.
INSERT INTO asset_engine_addresses (
    script_key, encoded, internal_key, creation_height, created_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (script_key) DO NOTHING;

-- name: ListAssetEngineAddresses :many
-- ListAssetEngineAddresses returns the addresses oldest first. Addresses
-- created at the same time keep the order of their insertion.
SELECT * FROM asset_engine_addresses ORDER BY created_at, address_id;

-- name: InsertAssetEngineReceive :execrows
-- InsertAssetEngineReceive records an announced transfer. A recorded
-- announcement inserts nothing.
INSERT INTO asset_engine_receives (
    outpoint_hash, outpoint_index, address_script_key, announcement, state,
    block_hash, block_height, created_at, payload
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (outpoint_hash, outpoint_index, address_script_key, announcement)
DO NOTHING;

-- name: GetAssetEngineReceive :one
SELECT * FROM asset_engine_receives
WHERE outpoint_hash = $1 AND outpoint_index = $2
    AND address_script_key = $3 AND announcement = $4;

-- name: UpdateAssetEngineReceive :execrows
UPDATE asset_engine_receives SET
    state = $5,
    block_hash = $6,
    block_height = $7,
    payload = $8
WHERE outpoint_hash = $1 AND outpoint_index = $2
    AND address_script_key = $3 AND announcement = $4;

-- name: ListAssetEngineReceives :many
SELECT * FROM asset_engine_receives ORDER BY created_at, receive_id;

-- name: ListAssetEngineReceivesByOutpoint :many
-- ListAssetEngineReceivesByOutpoint returns the announcements of one anchor
-- output, oldest first.
SELECT * FROM asset_engine_receives
WHERE outpoint_hash = $1 AND outpoint_index = $2
ORDER BY created_at, receive_id;
