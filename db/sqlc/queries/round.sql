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
SELECT * FROM rounds
WHERE status IN (
    'nonces_generated', 'nonces_aggregated', 'partial_sigs_sent',
    'forfeit_sigs_collecting', 'input_sig_sent'
)
ORDER BY creation_time ASC;

-- name: ListRoundsByStatus :many
SELECT * FROM rounds WHERE status = $1 ORDER BY creation_time DESC;

-- name: ListRoundsPaginated :many
-- ListRoundsPaginated returns rounds ordered by round_id with cursor-
-- based pagination. When cursor is empty, returns from the beginning.
SELECT * FROM rounds
WHERE (sqlc.arg(cursor) = '' OR round_id > sqlc.arg(cursor))
  AND (sqlc.arg(status_filter) = '' OR status = sqlc.arg(status_filter))
  AND (sqlc.arg(created_after) = 0 OR creation_time >= sqlc.arg(created_after))
  AND (sqlc.arg(created_before) = 0 OR creation_time <= sqlc.arg(created_before))
ORDER BY round_id ASC
LIMIT sqlc.arg(limit_count);

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
    exit_delay, policy_template, tx_proof, input_index, input_signature
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
    round_id, request_index, amount, pk_script, expiry, policy_template,
    client_pubkey, operator_pubkey, owner_key_family, owner_key_index,
    signing_key_family, signing_key_index, signing_pubkey
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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

-- Client round nonce state queries.

-- name: InsertClientRoundNonceState :exec
INSERT INTO client_round_nonce_state (
    round_id, signing_key, txid, pub_nonce, sec_nonce, creation_time,
    last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (round_id, signing_key, txid) DO UPDATE SET
    pub_nonce = excluded.pub_nonce,
    sec_nonce = excluded.sec_nonce,
    last_update_time = excluded.last_update_time;

-- name: GetClientRoundNonceState :many
SELECT * FROM client_round_nonce_state
WHERE round_id = $1
ORDER BY signing_key, txid;

-- Client round aggregate nonce state queries.

-- name: InsertClientRoundAggNonceState :exec
INSERT INTO client_round_agg_nonce_state (
    round_id, txid, agg_nonce, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (round_id, txid) DO UPDATE SET
    agg_nonce = excluded.agg_nonce,
    last_update_time = excluded.last_update_time;

-- name: GetClientRoundAggNonceState :many
SELECT * FROM client_round_agg_nonce_state
WHERE round_id = $1
ORDER BY txid;

-- Client round partial signature state queries.

-- name: InsertClientRoundPartialSigState :exec
INSERT INTO client_round_partial_sig_state (
    round_id, signing_key, txid, partial_sig, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (round_id, signing_key, txid) DO UPDATE SET
    partial_sig = excluded.partial_sig,
    last_update_time = excluded.last_update_time;

-- name: GetClientRoundPartialSigState :many
SELECT * FROM client_round_partial_sig_state
WHERE round_id = $1
ORDER BY signing_key, txid;

-- Client round collected VTXO forfeit signature queries.

-- name: InsertClientRoundForfeitSigState :exec
INSERT INTO client_round_forfeit_sig_state (
    round_id, vtxo_outpoint_hash, vtxo_outpoint_index, forfeit_tx,
    client_sig, spend_path, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (round_id, vtxo_outpoint_hash, vtxo_outpoint_index)
DO UPDATE SET
    forfeit_tx = excluded.forfeit_tx,
    client_sig = excluded.client_sig,
    spend_path = excluded.spend_path,
    last_update_time = excluded.last_update_time;

-- name: GetClientRoundForfeitSigState :many
SELECT * FROM client_round_forfeit_sig_state
WHERE round_id = $1
ORDER BY vtxo_outpoint_hash, vtxo_outpoint_index;

-- Client round expected VTXO forfeit request queries.

-- name: InsertClientRoundForfeitRequestState :exec
INSERT INTO client_round_forfeit_request_state (
    round_id, vtxo_outpoint_hash, vtxo_outpoint_index,
    connector_outpoint_hash, connector_outpoint_index, connector_pk_script,
    connector_amount, vtxo_amount, server_forfeit_pk_script, forfeit_spend,
    creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (round_id, vtxo_outpoint_hash, vtxo_outpoint_index)
DO UPDATE SET
    connector_outpoint_hash = excluded.connector_outpoint_hash,
    connector_outpoint_index = excluded.connector_outpoint_index,
    connector_pk_script = excluded.connector_pk_script,
    connector_amount = excluded.connector_amount,
    vtxo_amount = excluded.vtxo_amount,
    server_forfeit_pk_script = excluded.server_forfeit_pk_script,
    forfeit_spend = excluded.forfeit_spend,
    last_update_time = excluded.last_update_time;

-- name: GetClientRoundForfeitRequestState :many
SELECT * FROM client_round_forfeit_request_state
WHERE round_id = $1
ORDER BY vtxo_outpoint_hash, vtxo_outpoint_index;

-- Client round pending quote queries.

-- name: UpsertClientRoundPendingQuote :exec
INSERT INTO client_round_pending_quotes (
    round_id, quote_id, seal_pass, operator_fee_sat, quote_expires_at,
    reject_reason, creation_time, last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (round_id) DO UPDATE SET
    quote_id = excluded.quote_id,
    seal_pass = excluded.seal_pass,
    operator_fee_sat = excluded.operator_fee_sat,
    quote_expires_at = excluded.quote_expires_at,
    reject_reason = excluded.reject_reason,
    last_update_time = excluded.last_update_time;

-- name: DeleteClientRoundPendingVTXOQuotes :exec
DELETE FROM client_round_pending_vtxo_quotes WHERE round_id = $1;

-- name: InsertClientRoundPendingVTXOQuote :exec
INSERT INTO client_round_pending_vtxo_quotes (
    round_id, quote_index, pk_script, amount_sat, recipient_key
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (round_id, quote_index) DO UPDATE SET
    pk_script = excluded.pk_script,
    amount_sat = excluded.amount_sat,
    recipient_key = excluded.recipient_key;

-- name: DeleteClientRoundPendingLeaveQuotes :exec
DELETE FROM client_round_pending_leave_quotes WHERE round_id = $1;

-- name: InsertClientRoundPendingLeaveQuote :exec
INSERT INTO client_round_pending_leave_quotes (
    round_id, quote_index, pk_script, amount_sat
) VALUES ($1, $2, $3, $4)
ON CONFLICT (round_id, quote_index) DO UPDATE SET
    pk_script = excluded.pk_script,
    amount_sat = excluded.amount_sat;

-- name: ListClientRoundPendingQuotes :many
SELECT * FROM client_round_pending_quotes
ORDER BY creation_time ASC, round_id ASC;

-- name: GetClientRoundPendingVTXOQuotes :many
SELECT * FROM client_round_pending_vtxo_quotes
WHERE round_id = $1
ORDER BY quote_index ASC;

-- name: GetClientRoundPendingLeaveQuotes :many
SELECT * FROM client_round_pending_leave_quotes
WHERE round_id = $1
ORDER BY quote_index ASC;

-- name: DeleteClientRoundPendingQuote :exec
DELETE FROM client_round_pending_quotes WHERE round_id = $1;

-- Client round effect queries.

-- name: InsertClientRoundEffect :exec
INSERT INTO client_round_effects (
    id, round_id, effect_type, status, idempotency_key, attempts,
    max_attempts, next_attempt_at, created_at, updated_at
) VALUES ($1, $2, $3, 'pending', $4, 0, $5, $6, $7, $8)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ListDueClientRoundEffectIDs :many
SELECT id FROM client_round_effects
WHERE next_attempt_at <= $1
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $1)
  )
ORDER BY next_attempt_at, created_at, id
LIMIT $2;

-- name: ClaimClientRoundEffect :one
UPDATE client_round_effects
SET status = 'claimed',
    claim_owner = $2,
    claim_token = $3,
    claim_until = $4,
    attempts = attempts + 1,
    updated_at = $5
WHERE id = $1
  AND next_attempt_at <= $6
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $6)
  )
RETURNING id, round_id, effect_type, status, idempotency_key,
    attempts, max_attempts, next_attempt_at, claim_owner, claim_token,
    claim_until, last_error, created_at, updated_at, done_at;

-- name: MarkClientRoundEffectDone :exec
UPDATE client_round_effects
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

-- name: ReleaseClientRoundEffectForRetry :exec
UPDATE client_round_effects
SET status = CASE
        WHEN attempts >= max_attempts THEN 'dead'
        ELSE 'pending'
    END,
    next_attempt_at = $3,
    last_error = $4,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $5
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseExpiredClientRoundEffectClaims :exec
UPDATE client_round_effects
SET status = 'pending',
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $2
WHERE status = 'claimed'
  AND claim_until <= $1;

-- VTXO queries.

-- name: InsertVTXO :exec
-- InsertVTXO creates or updates a VTXO. On conflict, richer semantic and
-- metadata fields from the later insert win when present. This allows the
-- round store to create the initial row and the VTXO manager to heal it with
-- the finalized descriptor (policy template, key material, batch metadata).
INSERT INTO vtxos (
    outpoint_hash, outpoint_index, round_id, amount, pk_script, expiry,
    policy_template, client_key_family, client_key_index, client_pubkey,
    operator_pubkey, batch_expiry, chain_depth,
    created_height, commitment_txid, spent, creation_time, last_update_time
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18
)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE SET
    pk_script = CASE WHEN excluded.pk_script IS NOT NULL AND length(excluded.pk_script) > 0 THEN excluded.pk_script ELSE vtxos.pk_script END,
    expiry = CASE WHEN excluded.expiry != 0 THEN excluded.expiry ELSE vtxos.expiry END,
    policy_template = CASE WHEN excluded.policy_template IS NOT NULL AND length(excluded.policy_template) > 0 THEN excluded.policy_template ELSE vtxos.policy_template END,
    client_pubkey = CASE WHEN excluded.client_pubkey IS NOT NULL AND length(excluded.client_pubkey) > 0 THEN excluded.client_pubkey ELSE vtxos.client_pubkey END,
    client_key_family = CASE WHEN excluded.client_pubkey IS NOT NULL AND length(excluded.client_pubkey) > 0 THEN excluded.client_key_family ELSE vtxos.client_key_family END,
    client_key_index = CASE WHEN excluded.client_pubkey IS NOT NULL AND length(excluded.client_pubkey) > 0 THEN excluded.client_key_index ELSE vtxos.client_key_index END,
    operator_pubkey = CASE WHEN excluded.operator_pubkey IS NOT NULL AND length(excluded.operator_pubkey) > 0 THEN excluded.operator_pubkey ELSE vtxos.operator_pubkey END,
    batch_expiry = CASE WHEN excluded.batch_expiry != 0 THEN excluded.batch_expiry ELSE vtxos.batch_expiry END,
    chain_depth = CASE WHEN excluded.chain_depth != 0 THEN excluded.chain_depth ELSE vtxos.chain_depth END,
    created_height = CASE WHEN excluded.created_height != 0 THEN excluded.created_height ELSE vtxos.created_height END,
    commitment_txid = CASE WHEN excluded.commitment_txid IS NOT NULL AND length(excluded.commitment_txid) > 0 THEN excluded.commitment_txid ELSE vtxos.commitment_txid END,
    last_update_time = excluded.last_update_time;

-- name: InsertVTXOAncestryPath :exec
-- InsertVTXOAncestryPath inserts one ancestry tree fragment for a VTXO.
-- Callers replace the full set on update by deleting via
-- DeleteVTXOAncestryPaths first.
INSERT INTO vtxo_ancestry_paths (
    vtxo_outpoint_hash, vtxo_outpoint_index, path_order,
    commitment_txid, tree_path, tree_depth, input_indices
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: DeleteVTXOAncestryPaths :exec
-- DeleteVTXOAncestryPaths removes every ancestry row for the given VTXO.
-- Used as the first half of an upsert when the VTXO manager fills in
-- finalized lineage on top of a partially-written round-create row.
DELETE FROM vtxo_ancestry_paths
WHERE vtxo_outpoint_hash = $1 AND vtxo_outpoint_index = $2;

-- name: ListVTXOAncestryPaths :many
-- ListVTXOAncestryPaths returns the ancestry rows for one VTXO ordered by
-- path_order so the unroller sees the fragments in the same sequence the
-- indexer chose at materialization time.
SELECT * FROM vtxo_ancestry_paths
WHERE vtxo_outpoint_hash = $1 AND vtxo_outpoint_index = $2
ORDER BY path_order ASC;

-- name: ListLiveVTXOAncestryPaths :many
-- ListLiveVTXOAncestryPaths returns every ancestry row whose parent VTXO
-- is non-terminal, mirroring the filter on ListLiveVTXOs. Used as a
-- single batched companion query so descriptor materialization across
-- the live set runs in two queries total instead of N+1.
SELECT vap.* FROM vtxo_ancestry_paths vap
JOIN vtxos v
  ON v.outpoint_hash = vap.vtxo_outpoint_hash
  AND v.outpoint_index = vap.vtxo_outpoint_index
WHERE (v.status < 3 OR v.status = 7) AND v.spent = FALSE
ORDER BY vap.vtxo_outpoint_hash ASC,
         vap.vtxo_outpoint_index ASC,
         vap.path_order ASC;

-- name: ListVTXOAncestryPathsByStatus :many
-- ListVTXOAncestryPathsByStatus returns every ancestry row whose parent
-- VTXO matches the given status code. Companion to ListVTXOsByStatus.
SELECT vap.* FROM vtxo_ancestry_paths vap
JOIN vtxos v
  ON v.outpoint_hash = vap.vtxo_outpoint_hash
  AND v.outpoint_index = vap.vtxo_outpoint_index
WHERE v.status = $1
ORDER BY vap.vtxo_outpoint_hash ASC,
         vap.vtxo_outpoint_index ASC,
         vap.path_order ASC;

-- name: ListUnspentVTXOAncestryPaths :many
-- ListUnspentVTXOAncestryPaths returns every ancestry row whose parent
-- VTXO is unspent (status != 4 AND spent = FALSE), mirroring the filter
-- on ListUnspentVTXOs. Companion to the round-side ListVTXOs path.
SELECT vap.* FROM vtxo_ancestry_paths vap
JOIN vtxos v
  ON v.outpoint_hash = vap.vtxo_outpoint_hash
  AND v.outpoint_index = vap.vtxo_outpoint_index
WHERE v.spent = FALSE AND v.status != 4
ORDER BY vap.vtxo_outpoint_hash ASC,
         vap.vtxo_outpoint_index ASC,
         vap.path_order ASC;

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
