-- Round queries for server-side round persistence.

-- RoundStore queries.

-- name: InsertRound :exec
INSERT INTO rounds (
	round_id, final_tx, commitment_txid, confirmation_height,
	confirmation_block_hash, status, sweep_key, csv_delay, created_at,
	updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertRoundVTXOTree :exec
INSERT INTO round_vtxo_tree (round_id, batch_output_index)
VALUES ($1, $2);

-- name: InsertRoundConnectorDescriptor :exec
INSERT INTO round_connector_descriptors (
	round_id, output_index, num_leaves, forfeit_script
) VALUES ($1, $2, $3, $4);

-- name: InsertRoundClientRegistration :exec
INSERT INTO round_client_registrations (round_id, client_id, registration_data)
VALUES ($1, $2, $3);

-- name: InsertRoundForfeitInfo :exec
INSERT INTO round_forfeit_infos (
	round_id, outpoint_hash, outpoint_index, forfeit_tx,
	connector_output_index, leaf_index
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: UpsertRoundForfeitInfo :exec
INSERT INTO round_forfeit_infos (
	round_id, outpoint_hash, outpoint_index, forfeit_tx,
	connector_output_index, leaf_index
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (round_id, outpoint_hash, outpoint_index)
DO UPDATE SET
	forfeit_tx = EXCLUDED.forfeit_tx,
	connector_output_index = EXCLUDED.connector_output_index,
	leaf_index = EXCLUDED.leaf_index;

-- name: GetRound :one
SELECT * FROM rounds WHERE round_id = $1;

-- name: ListRoundsByIDsSqlite :many
SELECT * FROM rounds
WHERE round_id IN (sqlc.slice('round_ids')/*SLICE:round_ids*/);

-- name: ListRoundsByIDsPostgres :many
SELECT * FROM rounds
WHERE round_id = ANY(@round_ids::bytea[]);

-- name: GetRoundVTXOTrees :many
SELECT * FROM round_vtxo_tree
WHERE round_id = $1
ORDER BY batch_output_index ASC;

-- name: GetRoundConnectorDescriptors :many
SELECT * FROM round_connector_descriptors
WHERE round_id = $1
ORDER BY output_index ASC;

-- name: GetRoundClientRegistrations :many
SELECT * FROM round_client_registrations
WHERE round_id = $1;

-- name: GetRoundForfeitInfos :many
SELECT * FROM round_forfeit_infos
WHERE round_id = $1;

-- name: GetRoundForfeitInfoByOutpoint :many
SELECT * FROM round_forfeit_infos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListPendingRounds :many
SELECT * FROM rounds
WHERE status = 'pending'
ORDER BY created_at ASC;

-- name: ListAllRounds :many
SELECT * FROM rounds
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountAllRounds :one
SELECT count(*) FROM rounds;

-- name: ListRoundsByStatus :many
SELECT * FROM rounds
WHERE status = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountRoundsByStatus :one
SELECT count(*) FROM rounds
WHERE status = $1;

-- name: UpdateRoundConfirmed :exec
UPDATE rounds
SET status = 'confirmed',
	confirmation_height = $2,
	confirmation_block_hash = $3,
	updated_at = $4
WHERE round_id = $1;

-- VTXOStore queries.

-- name: InsertVTXO :exec
INSERT INTO vtxos (
	outpoint_hash, outpoint_index, round_id, batch_output_index,
	amount, pk_script, policy_template, cosigner_key, status,
	lock_owner_kind, lock_owner_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: InsertVTXOIfAbsent :execrows
INSERT INTO vtxos (
	outpoint_hash, outpoint_index, round_id, batch_output_index,
	amount, pk_script, policy_template, cosigner_key, status,
	lock_owner_kind, lock_owner_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT DO NOTHING;

-- name: GetVTXO :one
SELECT * FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: UpdateVTXOsLiveByRound :exec
UPDATE vtxos
SET status = 'live', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE round_id = $1 AND status = 'pending';

-- name: UpdateVTXOStatus :execrows
UPDATE vtxos
SET status = $3
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: MarkVTXOForfeited :execrows
UPDATE vtxos
SET status = 'forfeited', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: MarkVTXOUnrolledByClient :execrows
UPDATE vtxos
SET status = 'unrolled_by_client', lock_owner_kind = NULL,
    lock_owner_id = NULL
WHERE outpoint_hash = $1 AND outpoint_index = $2
  AND status IN ('live', 'in_flight');

-- name: MarkVTXOExpired :execrows
UPDATE vtxos
SET status = 'expired', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE outpoint_hash = $1 AND outpoint_index = $2
  AND status IN ('live', 'pending', 'in_flight');

-- name: LockVTXO :execrows
UPDATE vtxos
SET status = 'in_flight', lock_owner_kind = $3, lock_owner_id = $4
WHERE outpoint_hash = $1 AND outpoint_index = $2
	AND status = 'live'
	AND lock_owner_kind IS NULL
	AND lock_owner_id IS NULL;

-- name: UnlockVTXO :execrows
UPDATE vtxos
SET status = 'live', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE outpoint_hash = $1 AND outpoint_index = $2
	AND status = 'in_flight'
	AND lock_owner_kind = $3
	AND lock_owner_id = $4;

-- name: GetLockedVTXOs :many
SELECT * FROM vtxos
WHERE lock_owner_kind = $1
	AND lock_owner_id = $2;

-- name: UnlockStaleVTXOsSqlite :execrows
UPDATE vtxos
SET status = 'live', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE status = 'in_flight'
	AND lock_owner_kind = 'round'
	AND lock_owner_id IS NOT NULL
	AND lock_owner_id NOT IN (sqlc.slice('pending_round_ids'));

-- name: UnlockStaleVTXOsPostgres :execrows
UPDATE vtxos
SET status = 'live', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE status = 'in_flight'
	AND lock_owner_kind = 'round'
	AND lock_owner_id IS NOT NULL
	AND NOT (lock_owner_id = ANY(@pending_round_ids::bytea[]));

-- name: UnlockAllLockedVTXOs :execrows
UPDATE vtxos
SET status = 'live', lock_owner_kind = NULL, lock_owner_id = NULL
WHERE status = 'in_flight'
	AND lock_owner_kind = 'round'
	AND lock_owner_id IS NOT NULL;

-- name: ListVTXOsByRound :many
SELECT * FROM vtxos
WHERE round_id = $1
ORDER BY outpoint_hash, outpoint_index;

-- name: ListVTXOsByStatus :many
SELECT * FROM vtxos
WHERE status = $1
ORDER BY outpoint_hash, outpoint_index;

-- name: GetVTXOStatsByStatus :many
SELECT status, COUNT(*) AS count, COALESCE(SUM(amount), 0) AS total_value
FROM vtxos GROUP BY status;

-- name: GetRoundStatsByStatus :many
SELECT status, COUNT(*) AS count
FROM rounds GROUP BY status;

-- name: ListVTXOsByStatusPaged :many
SELECT * FROM vtxos WHERE status = $1
ORDER BY outpoint_hash, outpoint_index LIMIT $2 OFFSET $3;

-- name: CountVTXOsByStatus :one
SELECT count(*) FROM vtxos WHERE status = $1;

-- name: ListAllVTXOsPaged :many
SELECT * FROM vtxos
ORDER BY outpoint_hash, outpoint_index LIMIT $1 OFFSET $2;

-- name: CountAllVTXOs :one
SELECT count(*) FROM vtxos;

-- name: ListVTXOsByPkScriptsSqlite :many
SELECT * FROM vtxos
WHERE pk_script IN (sqlc.slice('pk_scripts')/*SLICE:pk_scripts*/)
ORDER BY outpoint_hash, outpoint_index;

-- name: ListVTXOsByPkScriptsPostgres :many
SELECT * FROM vtxos
WHERE pk_script = ANY(@pk_scripts::bytea[])
ORDER BY outpoint_hash, outpoint_index;
