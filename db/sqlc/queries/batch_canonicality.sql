-- Batch canonicality queries.
-- These maintain the durable, reorg-aware record of how each batch
-- (commitment) transaction is faring against the best chain, the inputs it
-- consumes, the VTXOs it anchors, and the reverse-dependency edges needed to
-- restore a provisionally consumed VTXO. The queries are behavior-free; all
-- interpretation lives in the batch canonicality manager.

-- name: UpsertBatchCanonicality :exec
-- UpsertBatchCanonicality inserts or replaces the canonicality row for a
-- batch. created_at is preserved on conflict; everything else is overwritten.
INSERT INTO batch_canonicality (
    batch_txid, state, confirmation_height, confirmation_block_hash,
    csv_expiry_delta, policy_state, created_at, updated_at,
    confirmation_pk_script
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (batch_txid) DO UPDATE SET
    state = EXCLUDED.state,
    confirmation_height = EXCLUDED.confirmation_height,
    confirmation_block_hash = EXCLUDED.confirmation_block_hash,
    csv_expiry_delta = EXCLUDED.csv_expiry_delta,
    policy_state = EXCLUDED.policy_state,
    confirmation_pk_script = EXCLUDED.confirmation_pk_script,
    updated_at = EXCLUDED.updated_at;

-- name: GetBatchCanonicality :one
-- GetBatchCanonicality returns the canonicality row for a batch txid. The
-- column order matches the table so sqlc reuses the BatchCanonicality model.
SELECT batch_txid, state, confirmation_height, confirmation_block_hash,
    csv_expiry_delta, policy_state, created_at, updated_at,
    confirmation_pk_script
FROM batch_canonicality
WHERE batch_txid = $1;

-- name: ListBatchCanonicalityByState :many
-- ListBatchCanonicalityByState returns every batch currently in the given
-- state.
SELECT batch_txid, state, confirmation_height, confirmation_block_hash,
    csv_expiry_delta, policy_state, created_at, updated_at,
    confirmation_pk_script
FROM batch_canonicality
WHERE state = $1;

-- name: UpdateBatchCanonicalityState :exec
-- UpdateBatchCanonicalityState transitions a batch to a new state without
-- touching its other fields.
UPDATE batch_canonicality
SET state = $2, updated_at = $3
WHERE batch_txid = $1;

-- name: RecordBatchConfirmation :exec
-- RecordBatchConfirmation records the best-chain height and block hash at
-- which the batch tx is confirmed. A later call at a different height (after
-- a reorg) overwrites the observation so effective expiry tracks the new
-- confirmation.
UPDATE batch_canonicality
SET confirmation_height = $2, confirmation_block_hash = $3, updated_at = $4
WHERE batch_txid = $1;

-- name: ClearBatchConfirmation :exec
-- ClearBatchConfirmation nulls the confirmation observation, reflecting that
-- the confirming block left the best chain. It sets no terminal flag.
UPDATE batch_canonicality
SET confirmation_height = NULL, confirmation_block_hash = NULL, updated_at = $2
WHERE batch_txid = $1;

-- name: InsertBatchConsumedInput :exec
-- InsertBatchConsumedInput records one input consumed by a batch, together
-- with the pkScript of the spent output (needed to register the spend watch).
INSERT INTO batch_consumed_inputs (
    batch_txid, input_hash, input_index, input_pk_script
)
VALUES ($1, $2, $3, $4)
ON CONFLICT (batch_txid, input_hash, input_index) DO NOTHING;

-- name: DeleteBatchConsumedInputs :exec
-- DeleteBatchConsumedInputs removes every consumed-input row for a batch,
-- used by the store's upsert to replace the set atomically.
DELETE FROM batch_consumed_inputs WHERE batch_txid = $1;

-- name: ListBatchConsumedInputs :many
-- ListBatchConsumedInputs returns the inputs a batch consumes, with the
-- pkScript of each spent output and its persisted conflict observation.
SELECT input_hash, input_index, input_pk_script, conflicting, conflict_final
FROM batch_consumed_inputs
WHERE batch_txid = $1;

-- name: RecordBatchInputConflict :exec
-- RecordBatchInputConflict persists the observed conflict status of one
-- consumed input, so restart reconciliation can rebuild the per-input
-- conflict view and not transiently downgrade a persisted conflict.
UPDATE batch_consumed_inputs
SET conflicting = $4, conflict_final = $5
WHERE batch_txid = $1 AND input_hash = $2 AND input_index = $3;

-- name: FindBatchesByConsumedOutpoint :many
-- FindBatchesByConsumedOutpoint returns the txids of every batch that
-- consumes the given outpoint.
SELECT batch_txid
FROM batch_consumed_inputs
WHERE input_hash = $1 AND input_index = $2;

-- name: InsertBatchDependentVTXO :exec
-- InsertBatchDependentVTXO records one VTXO outpoint anchored by a batch.
INSERT INTO batch_dependent_vtxos (
    batch_txid, vtxo_outpoint_hash, vtxo_outpoint_index
) VALUES ($1, $2, $3)
ON CONFLICT (batch_txid, vtxo_outpoint_hash, vtxo_outpoint_index) DO NOTHING;

-- name: DeleteBatchDependentVTXOs :exec
-- DeleteBatchDependentVTXOs removes every dependent-VTXO row for a batch,
-- used by the store's upsert to replace the set atomically.
DELETE FROM batch_dependent_vtxos WHERE batch_txid = $1;

-- name: ListBatchDependentVTXOs :many
-- ListBatchDependentVTXOs returns the VTXO outpoints a batch anchors.
SELECT vtxo_outpoint_hash, vtxo_outpoint_index
FROM batch_dependent_vtxos
WHERE batch_txid = $1;

-- name: InsertProvisionalConsumer :exec
-- InsertProvisionalConsumer records a reverse-dependency edge: consumed_vtxo
-- is provisionally consumed by consumer_batch. Idempotent.
INSERT INTO batch_provisional_consumers (
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid, created_at
) VALUES ($1, $2, $3, $4)
ON CONFLICT (
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid
) DO NOTHING;

-- name: ListProvisionalConsumersForBatch :many
-- ListProvisionalConsumersForBatch returns the VTXO outpoints that the given
-- consumer batch provisionally consumes (the VTXOs to restore if the batch
-- is invalidated).
SELECT consumed_vtxo_hash, consumed_vtxo_index
FROM batch_provisional_consumers
WHERE consumer_batch_txid = $1;

-- name: DeleteProvisionalConsumersForBatch :exec
-- DeleteProvisionalConsumersForBatch removes every reverse-dependency edge
-- for the given consumer batch.
DELETE FROM batch_provisional_consumers WHERE consumer_batch_txid = $1;

-- name: ListVTXOsForCanonicalityBackfill :many
-- ListVTXOsForCanonicalityBackfill returns the columns needed to derive
-- initial batch canonicality records from already-persisted VTXOs: each
-- VTXO's outpoint, its commitment (batch) txid, the absolute batch expiry
-- height, and the height at which it was created (confirmed). The backfill
-- groups these by commitment txid in Go and recomputes the CSV-relative
-- expiry delta as batch_expiry - created_height.
SELECT outpoint_hash, outpoint_index, commitment_txid, batch_expiry,
    created_height
FROM vtxos
WHERE length(commitment_txid) = 32;
