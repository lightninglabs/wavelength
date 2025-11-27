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
    status,
    creation_time,
    last_update_time
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE
SET
    amount = COALESCE(excluded.amount, boarding_intents.amount),
    status = excluded.status,
    conf_height = COALESCE(excluded.conf_height, boarding_intents.conf_height),
    conf_hash = COALESCE(excluded.conf_hash, boarding_intents.conf_hash),
    conf_tx = COALESCE(excluded.conf_tx, boarding_intents.conf_tx),
    last_update_time = excluded.last_update_time;

-- name: GetBoardingIntent :one
SELECT * FROM boarding_intents
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListBoardingIntentsByStatus :many
SELECT * FROM boarding_intents
WHERE status = $1
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
