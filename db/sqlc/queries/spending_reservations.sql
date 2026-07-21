-- Spending reservation queries.
-- These queries maintain a durable index of VTXO outpoints reserved by an
-- active spend owner (e.g. an outgoing OOR session) so a startup sweep can
-- release orphaned Spending VTXOs that have no live reservation.

-- name: UpsertSpendingReservation :exec
-- UpsertSpendingReservation records (or refreshes) the reservation for one
-- outpoint. The owner fields are updated on conflict so a resumed workflow
-- re-binds the same outpoint to its current durable owner.
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
-- ListSpendingReservationOutpoints returns every reserved outpoint. Used by
-- the startup sweep to build the set of live reservations.
SELECT outpoint_hash, outpoint_index FROM spending_reservations;
