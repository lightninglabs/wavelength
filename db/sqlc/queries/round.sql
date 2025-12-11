-- Round queries.

-- name: InsertRound :exec
INSERT INTO rounds (
    round_id, creation_height, commitment_tx, commitment_txid,
    vtxt_tree, status, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (round_id) DO UPDATE SET
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
SET status = 'confirmed', commitment_txid = $2, last_update_time = $3
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

-- Round VTXO templates queries.

-- name: InsertRoundVtxoTemplate :exec
INSERT INTO round_vtxo_templates (
    round_id, outpoint_hash, outpoint_index, template_index, amount, pk_script,
    expiry, client_pubkey, operator_pubkey, signing_key_family, signing_key_index,
    signing_pubkey
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (round_id, outpoint_hash, outpoint_index, template_index) DO NOTHING;

-- name: GetRoundVtxoTemplates :many
SELECT * FROM round_vtxo_templates
WHERE round_id = $1 AND outpoint_hash = $2 AND outpoint_index = $3
ORDER BY template_index ASC;

-- name: GetAllRoundVtxoTemplates :many
SELECT * FROM round_vtxo_templates
WHERE round_id = $1
ORDER BY outpoint_hash, outpoint_index, template_index ASC;

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
INSERT INTO vtxos (
    outpoint_hash, outpoint_index, round_id, amount, pk_script, expiry,
    client_key_family, client_key_index, client_pubkey, operator_pubkey,
    tree_path, spent, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (outpoint_hash, outpoint_index) DO NOTHING;

-- name: GetVTXO :one
SELECT * FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListAllVTXOs :many
SELECT * FROM vtxos ORDER BY creation_time DESC;

-- name: ListUnspentVTXOs :many
SELECT * FROM vtxos WHERE spent = FALSE ORDER BY creation_time DESC;

-- name: ListVTXOsByRound :many
SELECT * FROM vtxos WHERE round_id = $1 ORDER BY creation_time DESC;

-- name: MarkVTXOSpent :exec
UPDATE vtxos SET spent = TRUE, last_update_time = $3
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: CountUnspentVTXOs :one
SELECT COUNT(*) FROM vtxos WHERE spent = FALSE;

-- name: SumUnspentVTXOAmounts :one
SELECT COALESCE(SUM(amount), 0) as total FROM vtxos WHERE spent = FALSE;
