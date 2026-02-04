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
	amount, pk_script, cosigner_key, status, locked_by_round_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetVTXO :one
SELECT * FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: UpdateVTXOsLiveByRound :exec
UPDATE vtxos
SET status = 'live', locked_by_round_id = NULL
WHERE round_id = $1 AND status = 'pending';

-- name: UpdateVTXOStatus :execrows
UPDATE vtxos
SET status = $3
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: LockVTXO :execrows
UPDATE vtxos
SET status = 'locked', locked_by_round_id = $3
WHERE outpoint_hash = $1 AND outpoint_index = $2
	AND status = 'live'
	AND (locked_by_round_id IS NULL OR locked_by_round_id = $3);

-- name: UnlockVTXO :execrows
UPDATE vtxos
SET status = 'live', locked_by_round_id = NULL
WHERE outpoint_hash = $1 AND outpoint_index = $2
	AND status = 'locked'
	AND locked_by_round_id = $3;

-- name: GetLockedVTXOs :many
SELECT * FROM vtxos
WHERE locked_by_round_id = $1;

-- name: UnlockStaleVTXOs :execrows
UPDATE vtxos
SET status = 'live', locked_by_round_id = NULL
WHERE status = 'locked'
	AND locked_by_round_id IS NOT NULL
	AND locked_by_round_id NOT IN (sqlc.slice('pending_round_ids'));

-- name: UnlockAllLockedVTXOs :execrows
UPDATE vtxos
SET status = 'live', locked_by_round_id = NULL
WHERE status = 'locked'
	AND locked_by_round_id IS NOT NULL;

-- name: ListVTXOsByRound :many
SELECT * FROM vtxos
WHERE round_id = $1
ORDER BY outpoint_hash, outpoint_index;

-- name: ListVTXOsByStatus :many
SELECT * FROM vtxos
WHERE status = $1
ORDER BY outpoint_hash, outpoint_index;
