-- Spending reservation queries.
-- These queries maintain a durable index of VTXO outpoints reserved by an
-- active spend owner (e.g. an outgoing OOR session) so a startup sweep can
-- release orphaned Spending VTXOs that have no live reservation.

-- name: UpsertSpendingReservation :exec
-- UpsertSpendingReservation records (or refreshes) the reservation for one
-- outpoint. The owner fields are updated on conflict so a re-checkpointed
-- session re-binds the same outpoint to its current owner.
INSERT INTO spending_reservations (
    outpoint_hash, outpoint_index, owner_kind, owner_id, created_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE SET
    owner_kind = EXCLUDED.owner_kind,
    owner_id = EXCLUDED.owner_id,
    created_at = EXCLUDED.created_at;

-- name: DeleteSpendingReservation :exec
-- DeleteSpendingReservation removes the reservation for one outpoint. Called
-- when the VTXO leaves SpendingState (released or completed).
DELETE FROM spending_reservations
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: ListSpendingReservationOutpoints :many
-- ListSpendingReservationOutpoints returns every reserved outpoint, including
-- VTXOs held by a nonterminal virtual channel. Used by the startup sweep to
-- build the set of live reservations.
SELECT outpoint_hash, outpoint_index FROM spending_reservations
UNION
SELECT v.outpoint_hash, v.outpoint_index
FROM virtual_channel_vtxos AS v
JOIN virtual_channels AS c
  ON c.virtual_channel_id = v.virtual_channel_id
WHERE c.status != 'closed'
	AND (c.status != 'failed' OR c.backing_armed_at IS NOT NULL)
UNION
SELECT v.outpoint_hash, v.outpoint_index
FROM virtual_channel_intent_vtxos AS v
JOIN virtual_channel_intents AS i
  ON i.pending_channel_id = v.pending_channel_id
WHERE i.status != 'failed';
