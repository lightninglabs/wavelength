-- Boarding address queries.

-- name: InsertBoardingAddress :exec
INSERT INTO boarding_addresses (
    pk_script,
    address_string,
    client_pubkey,
    client_key_family,
    client_key_index,
    operator_pubkey,
    exit_delay,
    creation_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (pk_script) DO NOTHING;

-- name: GetBoardingAddress :one
SELECT * FROM boarding_addresses WHERE pk_script = $1;

-- name: ListAllBoardingAddresses :many
SELECT * FROM boarding_addresses ORDER BY creation_time DESC;

-- Boarding intent queries.

-- name: InsertBoardingIntent :exec
INSERT INTO boarding_intents (
    outpoint_hash,
    outpoint_index,
    pk_script,
    amount,
    conf_height,
    conf_hash,
    conf_tx,
    tx_proof,
    status,
    creation_time,
    last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE
SET
    amount = COALESCE(excluded.amount, boarding_intents.amount),
    status = excluded.status,
    conf_height = COALESCE(excluded.conf_height, boarding_intents.conf_height),
    conf_hash = COALESCE(excluded.conf_hash, boarding_intents.conf_hash),
    conf_tx = COALESCE(excluded.conf_tx, boarding_intents.conf_tx),
    -- tx_proof is preserved across re-inserts that carry no proof:
    -- a status-only upsert or a legacy reorg-replay must NOT null
    -- out a previously persisted SPV proof. The producer
    -- (domainIntentToInsertParams) normalises a zero-length proof
    -- slice to nil before the row is built, so excluded.tx_proof is
    -- either a populated blob or SQL NULL — plain COALESCE suffices
    -- and is portable across SQLite and Postgres BYTEA. Status, by
    -- contrast, is always authoritative on update so it overwrites
    -- without COALESCE.
    tx_proof = COALESCE(excluded.tx_proof, boarding_intents.tx_proof),
    last_update_time = excluded.last_update_time;

-- name: GetBoardingIntent :one
SELECT * FROM boarding_intents
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListBoardingIntentsByStatus :many
SELECT * FROM boarding_intents
WHERE status = $1
ORDER BY creation_time DESC;

-- name: ListBoardingIntentsBySweepableStatuses :many
SELECT * FROM boarding_intents
WHERE status IN ($1, $2, $3)
ORDER BY creation_time DESC;

-- name: ListAllBoardingIntents :many
SELECT * FROM boarding_intents
ORDER BY creation_time DESC;

-- name: ListBoardingIntentsByPkScript :many
SELECT * FROM boarding_intents
WHERE pk_script = $1
ORDER BY creation_time DESC;

-- name: ListBoardingIntentsByConfHeight :many
SELECT * FROM boarding_intents
WHERE conf_height >= $1
ORDER BY conf_height DESC;

-- name: UpdateBoardingIntentStatus :exec
UPDATE boarding_intents
SET status = $3, last_update_time = $4
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- Boarding sweep queries.

-- name: InsertBoardingSweep :exec
INSERT INTO boarding_sweeps (
    txid,
    raw_tx,
    destination_address,
    total_amount,
    fee_amount,
    fee_rate_sat_per_vbyte,
    vbytes,
    status,
    created_height,
    created_time,
    published_time,
    confirmed_height,
    last_error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (txid) DO UPDATE SET
    raw_tx = excluded.raw_tx,
    destination_address = excluded.destination_address,
    total_amount = excluded.total_amount,
    fee_amount = excluded.fee_amount,
    fee_rate_sat_per_vbyte = excluded.fee_rate_sat_per_vbyte,
    vbytes = excluded.vbytes,
    status = excluded.status,
    created_height = excluded.created_height,
    created_time = excluded.created_time,
    published_time = excluded.published_time,
    confirmed_height = excluded.confirmed_height,
    last_error = excluded.last_error;

-- name: InsertBoardingSweepInput :exec
INSERT INTO boarding_sweep_inputs (
    txid,
    outpoint_hash,
    outpoint_index,
    amount,
    previous_status,
    status,
    spent_by_txid,
    spent_height,
    last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (txid, outpoint_hash, outpoint_index) DO UPDATE SET
    amount = excluded.amount,
    previous_status = excluded.previous_status,
    status = excluded.status,
    spent_by_txid = excluded.spent_by_txid,
    spent_height = excluded.spent_height,
    last_update_time = excluded.last_update_time;

-- name: GetBoardingSweep :one
SELECT * FROM boarding_sweeps
WHERE txid = $1;

-- name: GetBoardingSweepByInput :one
SELECT bs.*
FROM boarding_sweeps bs
JOIN boarding_sweep_inputs bsi ON bsi.txid = bs.txid
WHERE bsi.outpoint_hash = $1
  AND bsi.outpoint_index = $2
  AND bsi.status IN ('pending', 'published');

-- name: ListPendingBoardingSweeps :many
SELECT * FROM boarding_sweeps
WHERE status IN ('pending', 'published')
ORDER BY created_time ASC;

-- name: ListBoardingSweeps :many
SELECT * FROM boarding_sweeps
WHERE (sqlc.arg(status_filter) = '' OR status = sqlc.arg(status_filter))
ORDER BY created_time DESC
LIMIT sqlc.arg(page_limit)
OFFSET sqlc.arg(page_offset);

-- name: ListBoardingSweepInputs :many
SELECT * FROM boarding_sweep_inputs
WHERE txid = $1
ORDER BY outpoint_hash ASC, outpoint_index ASC;

-- name: ListPendingBoardingSweepInputs :many
SELECT * FROM boarding_sweep_inputs
WHERE status IN ('pending', 'published')
ORDER BY last_update_time ASC;

-- name: MarkBoardingSweepStatus :exec
UPDATE boarding_sweeps
SET status = $2,
    published_time = COALESCE($3, published_time),
    confirmed_height = COALESCE($4, confirmed_height),
    last_error = $5
WHERE txid = $1;

-- name: MarkBoardingSweepInputStatus :exec
UPDATE boarding_sweep_inputs
SET status = $4,
    spent_by_txid = COALESCE($5, spent_by_txid),
    spent_height = COALESCE($6, spent_height),
    last_update_time = $7
WHERE txid = $1
  AND outpoint_hash = $2
  AND outpoint_index = $3;

-- name: MarkBoardingSweepInputsStatus :exec
UPDATE boarding_sweep_inputs
SET status = $2,
    last_update_time = $3
WHERE txid = $1
  AND status IN ('pending', 'published');

-- name: MarkBoardingSweepInputSpentByOutpoint :execrows
UPDATE boarding_sweep_inputs
SET status = $3,
    spent_by_txid = $4,
    spent_height = $5,
    last_update_time = $6
WHERE outpoint_hash = $1
  AND outpoint_index = $2
  AND status IN ('pending', 'published');

-- name: CountUnresolvedBoardingSweepInputs :one
SELECT COUNT(*) FROM boarding_sweep_inputs
WHERE txid = $1
  AND status IN ('pending', 'published');

-- name: CountBoardingIntentsByStatus :one
SELECT COUNT(*) FROM boarding_intents
WHERE status = $1;

-- name: SumBoardingIntentAmountsByStatus :one
SELECT COALESCE(SUM(amount), 0) as total FROM boarding_intents
WHERE status = $1;

-- name: ListBoardingIntentOutpoints :many
SELECT outpoint_hash, outpoint_index FROM boarding_intents;

-- name: ListBoardingIntentsByStatusAndMinHeight :many
SELECT * FROM boarding_intents
WHERE status = $1 AND conf_height >= $2
ORDER BY conf_height ASC;
