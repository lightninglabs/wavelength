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
SELECT sqlc.embed(vtxos),
    rounds.commitment_txid AS settlement_txid,
    rounds.confirmation_height AS settlement_height
FROM vtxos
LEFT JOIN rounds ON vtxos.forfeit_round_id = rounds.round_id
WHERE vtxos.status = $1
ORDER BY vtxos.creation_time DESC;

-- name: ListVTXOSelectionCandidatesByStatus :many
-- ListVTXOSelectionCandidatesByStatus returns the lightweight projection coin
-- selection runs on: outpoint, amount, and pkScript. Selection happens on
-- every payment and only needs these three fields, so this avoids decoding
-- full descriptors (pubkey parsing, taproot script reconstruction, policy
-- template decode) and the batched ancestry-path query on the hot path. A
-- VTXO assigned to a live channel lifecycle is unavailable to Ark coin
-- selection: while virtual it backs the unpublished channel point, and after
-- materialization that backing transaction has consumed it. A channel that
-- fails before activation releases its VTXO for a later attempt.
SELECT v.outpoint_hash, v.outpoint_index, v.amount, v.pk_script
FROM vtxos AS v
WHERE v.status = $1
		AND NOT EXISTS (
			SELECT 1
			FROM virtual_channel_vtxos AS cv
			JOIN virtual_channels AS c
				ON c.virtual_channel_id = cv.virtual_channel_id
			WHERE cv.outpoint_hash = v.outpoint_hash
				AND cv.outpoint_index = v.outpoint_index
				AND (c.status != 'failed' OR c.backing_armed_at IS NOT NULL)
		)
		AND NOT EXISTS (
			SELECT 1
			FROM virtual_channel_intent_vtxos AS iv
			JOIN virtual_channel_intents AS i
				ON i.pending_channel_id = iv.pending_channel_id
			WHERE iv.outpoint_hash = v.outpoint_hash
				AND iv.outpoint_index = v.outpoint_index
				AND i.status != 'failed'
		)
ORDER BY v.creation_time DESC;

-- name: ListLiveVTXOs :many
-- ListLiveVTXOs returns all VTXOs that are not in a terminal state.
-- Terminal states are: Forfeited (3), Spent (4), UnilateralExit (5),
-- Failed (6).
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

-- name: CountVTXOsByStatus :one
-- CountVTXOsByStatus returns the count of VTXOs with the specified status.
SELECT COUNT(*) FROM vtxos WHERE status = $1;
