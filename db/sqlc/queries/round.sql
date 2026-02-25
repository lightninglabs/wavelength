-- Round queries.

-- name: InsertRound :exec
INSERT INTO rounds (
    round_id, confirmation_height, confirmation_block_hash, commitment_tx,
    commitment_txid, vtxt_tree, status, creation_time, last_update_time,
    start_height
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (round_id) DO UPDATE SET
    confirmation_height = COALESCE(excluded.confirmation_height, rounds.confirmation_height),
    confirmation_block_hash = COALESCE(excluded.confirmation_block_hash, rounds.confirmation_block_hash),
    commitment_tx = COALESCE(excluded.commitment_tx, rounds.commitment_tx),
    commitment_txid = COALESCE(excluded.commitment_txid, rounds.commitment_txid),
    vtxt_tree = COALESCE(excluded.vtxt_tree, rounds.vtxt_tree),
    status = excluded.status,
    last_update_time = excluded.last_update_time;

-- name: GetRound :one
SELECT * FROM rounds WHERE round_id = $1;

-- name: GetRoundByCommitmentTxid :one
SELECT * FROM rounds WHERE commitment_txid = $1;

-- name: ListActiveRounds :many
SELECT * FROM rounds WHERE status = 'input_sig_sent' ORDER BY creation_time ASC;

-- name: ListRoundsByStatus :many
SELECT * FROM rounds WHERE status = $1 ORDER BY creation_time DESC;

-- name: UpdateRoundStatus :exec
UPDATE rounds
SET status = $2, last_update_time = $3
WHERE round_id = $1;

-- name: FinalizeRound :exec
UPDATE rounds
SET status = 'confirmed',
    commitment_txid = $2,
    confirmation_height = $3,
    confirmation_block_hash = $4,
    last_update_time = $5
WHERE round_id = $1;

-- Round boarding intents queries.

-- name: InsertRoundBoardingIntent :exec
INSERT INTO round_boarding_intents (
    round_id, outpoint_hash, outpoint_index, client_key, operator_key,
    exit_delay, tx_proof, input_index, input_signature
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (round_id, outpoint_hash, outpoint_index) DO UPDATE SET
    input_index = COALESCE(excluded.input_index, round_boarding_intents.input_index),
    input_signature = COALESCE(excluded.input_signature, round_boarding_intents.input_signature);

-- name: GetRoundBoardingIntents :many
SELECT * FROM round_boarding_intents WHERE round_id = $1;

-- name: UpdateRoundBoardingIntentSignature :exec
UPDATE round_boarding_intents
SET input_signature = $4, input_index = $5
WHERE round_id = $1 AND outpoint_hash = $2 AND outpoint_index = $3;

-- Round VTXO request queries.

-- name: InsertRoundVtxoRequest :exec
INSERT INTO round_vtxo_requests (
    round_id, request_index, amount, pk_script, expiry, client_pubkey,
    operator_pubkey, signing_key_family, signing_key_index, signing_pubkey
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
    ON CONFLICT (round_id, request_index) DO NOTHING;

-- name: GetRoundVtxoRequests :many
SELECT * FROM round_vtxo_requests
WHERE round_id = $1
ORDER BY request_index ASC;

-- Client trees queries.

-- name: InsertRoundClientTree :exec
INSERT INTO round_client_trees (round_id, client_key, tree_data)
VALUES ($1, $2, $3)
ON CONFLICT (round_id, client_key) DO UPDATE
SET tree_data = excluded.tree_data;

-- name: GetRoundClientTrees :many
SELECT * FROM round_client_trees WHERE round_id = $1;

-- name: GetRoundClientTree :one
SELECT * FROM round_client_trees
WHERE round_id = $1 AND client_key = $2;

-- Client tree txids queries.

-- name: InsertClientTreeTxid :exec
INSERT INTO client_tree_txids (txid, round_id, client_key, tree_level, output_index)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (txid, round_id, client_key) DO NOTHING;

-- name: GetClientTreeByTxid :one
SELECT t.* FROM round_client_trees t
JOIN client_tree_txids idx ON t.round_id = idx.round_id AND t.client_key = idx.client_key
WHERE idx.txid = $1;

-- name: GetClientTreeTxidInfo :one
SELECT * FROM client_tree_txids WHERE txid = $1;

-- name: GetClientTreeTxids :many
SELECT txid, tree_level, output_index FROM client_tree_txids
WHERE round_id = $1 AND client_key = $2
ORDER BY tree_level ASC;

-- name: DeleteClientTreeTxids :exec
DELETE FROM client_tree_txids WHERE round_id = $1 AND client_key = $2;

-- VTXO queries.

-- name: InsertVTXO :exec
-- InsertVTXO creates or updates a VTXO. On conflict, metadata fields are
-- updated if the new values are non-zero/non-null (allowing the VTXO manager
-- to fill in BatchExpiry, TreeDepth, CreatedHeight, CommitmentTxid after the
-- round store creates the initial record).
INSERT INTO vtxos (
    outpoint_hash, outpoint_index, round_id, amount, pk_script, expiry,
    client_key_family, client_key_index, client_pubkey, operator_pubkey,
    tree_path, batch_expiry, tree_depth, created_height, commitment_txid,
    spent, creation_time, last_update_time
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18
)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE SET
    batch_expiry = CASE WHEN excluded.batch_expiry != 0 THEN excluded.batch_expiry ELSE vtxos.batch_expiry END,
    tree_depth = CASE WHEN excluded.tree_depth != 0 THEN excluded.tree_depth ELSE vtxos.tree_depth END,
    created_height = CASE WHEN excluded.created_height != 0 THEN excluded.created_height ELSE vtxos.created_height END,
    commitment_txid = CASE WHEN excluded.commitment_txid IS NOT NULL AND length(excluded.commitment_txid) > 0 THEN excluded.commitment_txid ELSE vtxos.commitment_txid END,
    last_update_time = excluded.last_update_time;

-- name: GetVTXO :one
SELECT * FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListAllVTXOs :many
SELECT * FROM vtxos ORDER BY creation_time DESC;

-- name: ListUnspentVTXOs :many
-- Unspent requires both spent=false and status!=Spent(4).
SELECT * FROM vtxos
WHERE spent = FALSE
    AND status != 4
ORDER BY creation_time DESC;

-- name: ListVTXOsByRound :many
SELECT * FROM vtxos WHERE round_id = $1 ORDER BY creation_time DESC;

-- name: MarkVTXOSpent :exec
-- Also sets status = 4 (Spent) to keep status in sync with spent flag.
UPDATE vtxos SET spent = TRUE, status = 4, last_update_time = $3
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: CountUnspentVTXOs :one
SELECT COUNT(*) FROM vtxos
WHERE spent = FALSE
    AND status != 4;

-- name: SumUnspentVTXOAmounts :one
SELECT COALESCE(SUM(amount), 0) as total FROM vtxos
WHERE spent = FALSE
    AND status != 4;
