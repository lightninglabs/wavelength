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
    batch_txid, batch_tx, batch_output_index, state, registration_stage,
    observation_generation, ready_generation, revision,
    confirmation_height, confirmation_block_hash, csv_expiry_delta,
    policy_state, created_at, updated_at, confirmation_pk_script
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15
)
ON CONFLICT (batch_txid) DO UPDATE SET
    batch_tx = EXCLUDED.batch_tx,
    batch_output_index = EXCLUDED.batch_output_index,
    state = EXCLUDED.state,
    registration_stage = EXCLUDED.registration_stage,
    observation_generation = EXCLUDED.observation_generation,
    ready_generation = EXCLUDED.ready_generation,
    revision = EXCLUDED.revision,
    confirmation_height = EXCLUDED.confirmation_height,
    confirmation_block_hash = EXCLUDED.confirmation_block_hash,
    csv_expiry_delta = EXCLUDED.csv_expiry_delta,
    policy_state = EXCLUDED.policy_state,
    confirmation_pk_script = EXCLUDED.confirmation_pk_script,
    updated_at = EXCLUDED.updated_at;

-- name: GetBatchCanonicality :one
-- GetBatchCanonicality returns the canonicality row for a batch txid. The
-- column order matches the table so sqlc reuses the BatchCanonicality model.
SELECT batch_txid, batch_tx, batch_output_index, state, registration_stage,
    observation_generation, ready_generation, revision,
    confirmation_height, confirmation_block_hash, csv_expiry_delta,
    policy_state, created_at, updated_at, confirmation_pk_script
FROM batch_canonicality
WHERE batch_txid = $1;

-- name: ListBatchCanonicalityByState :many
-- ListBatchCanonicalityByState returns every batch currently in the given
-- state.
SELECT batch_txid, batch_tx, batch_output_index, state, registration_stage,
    observation_generation, ready_generation, revision,
    confirmation_height, confirmation_block_hash, csv_expiry_delta,
    policy_state, created_at, updated_at, confirmation_pk_script
FROM batch_canonicality
WHERE state = $1;

-- name: BeginBatchCanonicalityReconcile :one
-- BeginBatchCanonicalityReconcile closes admission and starts a fresh
-- observation generation before any watch is armed.
UPDATE batch_canonicality
SET registration_stage = 1,
    observation_generation = observation_generation + 1,
    ready_generation = NULL,
    revision = revision + 1,
    updated_at = $2
WHERE batch_txid = $1
RETURNING batch_txid, batch_tx, batch_output_index, state,
    registration_stage, observation_generation, ready_generation, revision,
    confirmation_height, confirmation_block_hash, csv_expiry_delta,
    policy_state, created_at, updated_at, confirmation_pk_script;

-- name: MarkBatchCanonicalityReady :execrows
-- MarkBatchCanonicalityReady opens admission only for the generation whose
-- complete snapshot was installed. A stale generation updates zero rows.
UPDATE batch_canonicality
SET registration_stage = 2,
    ready_generation = $2,
    revision = revision + 1,
    updated_at = $3
WHERE batch_txid = $1 AND observation_generation = $2;

-- name: ApplyBatchCanonicalityObservation :execrows
-- ApplyBatchCanonicalityObservation atomically installs the batch-level part
-- of one complete observation snapshot. The caller updates every input in the
-- same SQL transaction before this generation-guarded write.
UPDATE batch_canonicality
SET state = $3,
    confirmation_height = $4,
    confirmation_block_hash = $5,
    registration_stage = CASE
        WHEN COALESCE($6, CAST(-1 AS BIGINT)) = CAST(-1 AS BIGINT)
            THEN registration_stage
        ELSE 2
    END,
    ready_generation = $6,
    revision = revision + 1,
    updated_at = $7
WHERE batch_txid = $1 AND observation_generation = $2
    AND registration_stage != 3;

-- name: QuarantineBatchCanonicality :exec
-- QuarantineBatchCanonicality fails a record closed after contradictory
-- immutable evidence is presented.
UPDATE batch_canonicality
SET registration_stage = 3,
    ready_generation = NULL,
    revision = revision + 1,
    updated_at = $2
WHERE batch_txid = $1;

-- name: UpdateBatchCanonicalityState :exec
-- UpdateBatchCanonicalityState transitions a batch to a new state without
-- touching its other fields.
UPDATE batch_canonicality
SET state = $2, revision = revision + 1, updated_at = $3
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
    batch_txid, input_hash, input_index, input_value, input_pk_script
)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (batch_txid, input_hash, input_index) DO NOTHING;

-- name: DeleteBatchConsumedInputs :exec
-- DeleteBatchConsumedInputs removes every consumed-input row for a batch,
-- used by the store's upsert to replace the set atomically.
DELETE FROM batch_consumed_inputs WHERE batch_txid = $1;

-- name: ListBatchConsumedInputs :many
-- ListBatchConsumedInputs returns the inputs a batch consumes, with the
-- pkScript of each spent output and its persisted conflict observation.
SELECT input_hash, input_index, input_value, input_pk_script, conflicting,
    conflict_final
FROM batch_consumed_inputs
WHERE batch_txid = $1;

-- name: RecordBatchInputConflict :execrows
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
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid,
    expected_vtxo_revision, created_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid
) DO NOTHING;

-- name: GetProvisionalConsumer :one
-- GetProvisionalConsumer returns the immutable expected business revision of
-- one edge so repeat registration can reject contradictory evidence.
SELECT expected_vtxo_revision
FROM batch_provisional_consumers
WHERE consumed_vtxo_hash = $1 AND consumed_vtxo_index = $2
    AND consumer_batch_txid = $3;

-- name: InsertConsumerCreatorLineage :exec
INSERT INTO batch_consumer_creator_lineage (
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid,
    creator_batch_txid
) VALUES ($1, $2, $3, $4)
ON CONFLICT (
    consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid,
    creator_batch_txid
) DO NOTHING;

-- name: ListConsumerCreatorLineage :many
SELECT creator_batch_txid
FROM batch_consumer_creator_lineage
WHERE consumed_vtxo_hash = $1 AND consumed_vtxo_index = $2
    AND consumer_batch_txid = $3
ORDER BY creator_batch_txid;

-- name: ListProvisionalConsumersForBatch :many
-- ListProvisionalConsumersForBatch returns every pending edge and expected
-- business revision owned by one consumer batch.
SELECT consumed_vtxo_hash, consumed_vtxo_index, expected_vtxo_revision
FROM batch_provisional_consumers
WHERE consumer_batch_txid = $1;

-- name: ListPendingConsumerBatchesByCreator :many
-- ListPendingConsumerBatchesByCreator targets durable restore checkpoints
-- whose creator-lineage decision may change when this batch changes state.
SELECT DISTINCT consumer_batch_txid
FROM batch_consumer_creator_lineage
WHERE creator_batch_txid = $1
ORDER BY consumer_batch_txid;

-- name: DeleteProvisionalConsumer :execrows
-- DeleteProvisionalConsumer completes one exact edge. Its normalized creator
-- lineage cascades with it.
DELETE FROM batch_provisional_consumers
WHERE consumed_vtxo_hash = $1 AND consumed_vtxo_index = $2
    AND consumer_batch_txid = $3
    AND expected_vtxo_revision = $4;

-- name: RestoreForfeitedVTXOForConsumer :execrows
-- RestoreForfeitedVTXOForConsumer is the business-state CAS. The caller
-- deletes the exact edge in the same transaction only when this updates one
-- row. A competing viable consumer, reservation, completed spend, different
-- consumer marker, or stale revision makes it update zero rows.
UPDATE vtxos
SET status = 0,
    forfeit_round_id = NULL,
    forfeit_tx = NULL,
    forfeit_txid = NULL,
    forfeit_consumer_txid = NULL,
    replaced_by_hash = NULL,
    replaced_by_index = NULL,
    business_revision = business_revision + 1,
    last_update_time = $5
WHERE vtxos.outpoint_hash = $1 AND vtxos.outpoint_index = $2
    AND vtxos.status = 3 AND vtxos.spent = FALSE
    AND vtxos.business_revision = $3
    AND vtxos.forfeit_consumer_txid = $4
    AND EXISTS (
        SELECT 1
        FROM batch_canonicality consumer_batch
        WHERE consumer_batch.batch_txid = $4
            AND consumer_batch.state = 5
            AND consumer_batch.registration_stage = 2
            AND consumer_batch.ready_generation =
                consumer_batch.observation_generation
    )
    AND NOT EXISTS (
        SELECT 1 FROM spending_reservations reservation
        WHERE reservation.outpoint_hash = $1
            AND reservation.outpoint_index = $2
    )
    AND NOT EXISTS (
        SELECT 1
        FROM batch_provisional_consumers other_edge
        JOIN batch_canonicality other_batch
            ON other_batch.batch_txid = other_edge.consumer_batch_txid
        WHERE other_edge.consumed_vtxo_hash = $1
            AND other_edge.consumed_vtxo_index = $2
            AND other_edge.consumer_batch_txid != $4
            AND NOT (
                other_batch.state = 5
                AND other_batch.registration_stage = 2
                AND other_batch.ready_generation =
                    other_batch.observation_generation
            )
    );

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
