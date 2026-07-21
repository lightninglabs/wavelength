-- VTXO status and lifecycle queries.
-- These queries support the vtxo.VTXOStore interface for VTXO lifecycle
-- management, including status transitions and forfeit transaction tracking.

-- name: ListVTXOsByStatus :many
-- ListVTXOsByStatus returns all VTXOs with the specified status. It also
-- LEFT JOINs the round that forfeited each VTXO (via forfeit_round_id) so a
-- FORFEITED VTXO can surface the settling commitment txid and its confirmation
-- height. The join columns are NULL for every VTXO whose forfeit round is
-- unknown (all non-forfeited VTXOs, and forfeited ones whose round row is
-- absent), so consumers must treat them as optional.
--
-- settlement_fee_sat is the TOTAL operator fee the client's ledger booked for
-- the forfeit round (boarding_fee_paid + refresh_fee_paid, joined via the
-- round_uuid TEXT mirror of the ledger's BLOB round_id). Every VTXO forfeited
-- in the same round reports the same round-level figure — consumers must not
-- sum it across VTXOs. Zero when the forfeit round is unknown or its fee rows
-- are absent (e.g. rows written before the round_uuid backfill ran).
--
-- The fee lookup is a correlated scalar subquery rather than a grouped join:
-- the planner then resolves it as a per-row probe of the
-- idx_client_ledger_round_uuid index for the (few) forfeited rows that carry
-- a forfeit_round_id, instead of aggregating every fee row in the ledger on
-- every call.
SELECT sqlc.embed(vtxos),
    rounds.commitment_txid AS settlement_txid,
    rounds.confirmation_height AS settlement_height,
    CAST(COALESCE((
        SELECT SUM(le.amount_sat)
        FROM ledger_entries AS le
        WHERE le.round_uuid = vtxos.forfeit_round_id
          AND le.event_type IN ('boarding_fee_paid', 'refresh_fee_paid')
    ), 0) AS BIGINT) AS settlement_fee_sat
FROM vtxos
LEFT JOIN rounds ON vtxos.forfeit_round_id = rounds.round_id
WHERE vtxos.status = $1
ORDER BY vtxos.creation_time DESC;

-- name: ListVTXOSelectionCandidatesByStatus :many
-- ListVTXOSelectionCandidatesByStatus returns the lightweight projection coin
-- selection runs on: outpoint, amount, and pkScript. Selection happens on
-- every payment and only needs these three fields, so this avoids decoding
-- full descriptors (pubkey parsing, taproot script reconstruction, policy
-- template decode) and the batched ancestry-path query on the hot path.
SELECT outpoint_hash, outpoint_index, amount, pk_script
FROM vtxos
WHERE status = $1
ORDER BY creation_time DESC;

-- name: ListLiveVTXOs :many
-- ListLiveVTXOs returns all VTXOs that are not in a terminal state.
-- Terminal states are: Forfeited (3), Spent (4), UnilateralExit (5),
-- Failed (6), Expired (8), Redeeming (9), Redeemed (10).
-- Non-terminal states: Live (0), PendingForfeit (1), Forfeiting (2),
-- Spending (7).
-- This is used during startup to recover active VTXO actors.
-- Also filter on spent = FALSE to handle VTXOs marked spent via the earlier
-- flag before the status field was introduced.
SELECT * FROM vtxos
WHERE (status < 3 OR status = 7) AND spent = FALSE
ORDER BY creation_time DESC;

-- name: UpdateVTXOStatus :exec
-- UpdateVTXOStatus atomically updates a VTXO's status. This is the primary
-- method for state transitions that don't require additional data.
UPDATE vtxos
SET status = $3,
    -- Keep spent flag in sync when status transitions to Spent (4).
    -- We intentionally do not clear spent once set.
    spent = CASE WHEN $3 = 4 THEN TRUE ELSE spent END,
    last_update_time = $4
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: RecoverLegacyExpiredVTXOs :many
-- Older clients could persist a locally expired live VTXO as Failed. Recover
-- only rows with the exact live-era shape: unspent, past their absolute
-- expiry, and with no forfeit, replacement, or redemption lifecycle metadata.
-- The metadata predicates deliberately leave unrelated Failed rows untouched.
UPDATE vtxos
SET status = 8, -- Expired
    last_update_time = sqlc.arg(last_update_time)
WHERE status = 6 -- Failed
    AND spent = FALSE
    AND batch_expiry > 0
    AND batch_expiry <= sqlc.arg(best_height)
    AND round_id <> ''
    AND amount > 0
    AND length(pk_script) > 0
    AND length(operator_pubkey) = 33
    AND length(commitment_txid) = 32
    AND forfeit_round_id IS NULL
    AND forfeit_tx IS NULL
    AND forfeit_txid IS NULL
    AND replaced_by_hash IS NULL
    AND replaced_by_index IS NULL
    AND redemption_round_id IS NULL
RETURNING outpoint_hash, outpoint_index;

-- name: MarkVTXOForfeiting :exec
-- MarkVTXOForfeiting transitions a VTXO to Forfeiting status and persists
-- the forfeit round ID and transaction for crash recovery. Called when
-- entering the forfeit flow.
UPDATE vtxos
SET status = 2, -- Forfeiting
    forfeit_round_id = $3,
    forfeit_tx = $4,
    last_update_time = $5
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListForfeitingVTXOsByRound :many
-- ListForfeitingVTXOsByRound returns the outpoint and amount of every VTXO
-- sitting in Forfeiting status whose forfeit reservation is bound to the
-- given round. Used during restart recovery to rebuild a reloaded round's
-- forfeit set, so the status-reconcile release path has real outpoints to
-- return to Live rather than the empty in-memory set the crash discarded.
SELECT outpoint_hash, outpoint_index, amount
FROM vtxos
WHERE status = 2 -- Forfeiting
  AND forfeit_round_id = $1;

-- name: GetVTXOForfeitTx :one
-- GetVTXOForfeitTx retrieves the persisted forfeit transaction for a VTXO.
-- Used during recovery to restore the ForfeitingState with its tx.
SELECT forfeit_tx, forfeit_round_id FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: MarkVTXOForfeited :exec
-- MarkVTXOForfeited marks a VTXO as forfeited and records the forfeit
-- transaction ID and replacement VTXO outpoint. Called when the new round's
-- commitment transaction confirms.
UPDATE vtxos
SET status = 3, -- Forfeited
    forfeit_txid = $3,
    replaced_by_hash = $4,
    replaced_by_index = $5,
    last_update_time = $6
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: DeleteVTXO :exec
-- DeleteVTXO removes a VTXO from storage. Used for cleanup after terminal
-- states are reached and the VTXO is no longer needed.
DELETE FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: GetVTXOReplacement :one
-- GetVTXOReplacement retrieves the replacement VTXO outpoint for a forfeited
-- VTXO. Returns NULL if not forfeited or no replacement recorded.
SELECT replaced_by_hash, replaced_by_index FROM vtxos
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: MarkVTXORedeeming :execrows
-- MarkVTXORedeeming records the round point-of-no-return for an expired
-- claim. Restricting the source status prevents a stale callback from
-- resurrecting a spent, forfeited, or already-redeemed VTXO.
UPDATE vtxos
SET status = 9, -- Redeeming
    redemption_round_id = $3,
    last_update_time = $4
WHERE outpoint_hash = $1 AND outpoint_index = $2
    AND (
        (status = 8 AND redemption_round_id IS NULL) OR
        (status = 9 AND redemption_round_id = $3)
    );

-- name: RevertVTXORedeeming :execrows
-- RevertVTXORedeeming returns a claim to the retryable Expired state after
-- its adopted round terminates without producing a replacement.
UPDATE vtxos
SET status = 8, -- Expired
    redemption_round_id = NULL,
    last_update_time = $4
WHERE outpoint_hash = $1 AND outpoint_index = $2 AND status = 9
    AND redemption_round_id = $3;

-- name: MarkVTXORedeemed :execrows
-- MarkVTXORedeemed links an authoritative, policy-validated completed mapping
-- to its replacement. A completed mapping may supersede an abandoned local
-- Redeeming round, so Expired/Redeeming sources adopt the replacement's round
-- ID. Once Redeemed, only an exact round/replacement replay is accepted.
UPDATE vtxos
SET status = 10, -- Redeemed
    replaced_by_hash = sqlc.arg(replaced_by_hash),
    replaced_by_index = sqlc.arg(replaced_by_index),
    redemption_round_id = sqlc.arg(redemption_round_id),
    last_update_time = sqlc.arg(last_update_time)
WHERE outpoint_hash = sqlc.arg(outpoint_hash)
    AND outpoint_index = sqlc.arg(outpoint_index)
    AND (
        status = 8 OR
        status = 9 OR
        (status = 10 AND
            redemption_round_id = sqlc.arg(redemption_round_id))
    )
    AND (
        replaced_by_hash IS NULL OR
        (replaced_by_hash = sqlc.arg(replaced_by_hash) AND
            replaced_by_index = sqlc.arg(replaced_by_index))
    );

-- name: InsertVTXORedemptionOutbox :execrows
-- InsertVTXORedemptionOutbox persists the idempotent observer work in the
-- same transaction that retires the source. A replay with the same source is
-- validated by the store against GetVTXORedemptionOutbox.
INSERT INTO vtxo_redemption_outbox (
    source_hash, source_index, replacement_hash, replacement_index,
    redemption_round_id, creation_time
) VALUES (
    sqlc.arg(source_hash), sqlc.arg(source_index),
    sqlc.arg(replacement_hash), sqlc.arg(replacement_index),
    sqlc.arg(redemption_round_id), sqlc.arg(creation_time)
)
ON CONFLICT (source_hash, source_index) DO NOTHING;

-- name: GetVTXORedemptionOutbox :one
-- GetVTXORedemptionOutbox loads one pending observer record for conflict
-- validation and idempotent acknowledgement.
SELECT source_hash, source_index, replacement_hash, replacement_index,
    redemption_round_id
FROM vtxo_redemption_outbox
WHERE source_hash = sqlc.arg(source_hash)
    AND source_index = sqlc.arg(source_index);

-- name: ListVTXORedemptionOutbox :many
-- ListVTXORedemptionOutbox is the durable startup/runtime replay queue.
SELECT source_hash, source_index, replacement_hash, replacement_index,
    redemption_round_id
FROM vtxo_redemption_outbox
ORDER BY creation_time ASC;

-- name: DeleteVTXORedemptionOutbox :execrows
-- DeleteVTXORedemptionOutbox acknowledges observer work with an exact
-- source/replacement/round CAS so a stale callback cannot clear newer work.
DELETE FROM vtxo_redemption_outbox
WHERE source_hash = sqlc.arg(source_hash)
    AND source_index = sqlc.arg(source_index)
    AND replacement_hash = sqlc.arg(replacement_hash)
    AND replacement_index = sqlc.arg(replacement_index)
    AND redemption_round_id = sqlc.arg(redemption_round_id);

-- name: CountVTXOsByStatus :one
-- CountVTXOsByStatus returns the count of VTXOs with the specified status.
SELECT COUNT(*) FROM vtxos WHERE status = $1;
