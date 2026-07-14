package db

import (
	"context"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
)

// SpendingReservationStore groups the SQL methods needed to maintain the
// durable spending-reservation index.
type SpendingReservationStore interface {
	UpsertSpendingReservation(ctx context.Context,
		arg sqlc.UpsertSpendingReservationParams) error

	ListSpendingReservationOutpoints(ctx context.Context) (
		[]sqlc.ListSpendingReservationOutpointsRow, error)
}

// BatchedSpendingReservationStore combines the query surface with batched
// transaction execution.
type BatchedSpendingReservationStore interface {
	SpendingReservationStore
	BatchedTx[SpendingReservationStore]
}

// SpendingReservationPersistenceStore persists the durable index of VTXO
// outpoints reserved by an active spend owner (e.g. an outgoing OOR session).
// A row exists IFF the owning session was durably checkpointed, so a startup
// sweep can deterministically release orphaned Spending VTXOs that have no
// reservation row.
type SpendingReservationPersistenceStore struct {
	db    BatchedSpendingReservationStore
	clock clock.Clock
}

// NewSpendingReservationPersistenceStore creates a spending-reservation store
// using the transaction executor pattern.
func NewSpendingReservationPersistenceStore(
	db BatchedSpendingReservationStore, clk clock.Clock,
) *SpendingReservationPersistenceStore {

	return &SpendingReservationPersistenceStore{
		db:    db,
		clock: clk,
	}
}

// UpsertReservation records (or refreshes) the reservation for one outpoint.
func (s *SpendingReservationPersistenceStore) UpsertReservation(
	ctx context.Context, outpoint wire.OutPoint, ownerKind int,
	ownerID chainhash.Hash,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(
		q SpendingReservationStore) error {

		params := sqlc.UpsertSpendingReservationParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			OwnerKind:     int32(ownerKind),
			OwnerID:       ownerID[:],
			CreatedAt:     s.clock.Now().Unix(),
		}

		return q.UpsertSpendingReservation(ctx, params)
	})
}

// ListReservedOutpoints returns every reserved outpoint. Used by the startup
// sweep to build the set of live reservations.
func (s *SpendingReservationPersistenceStore) ListReservedOutpoints(
	ctx context.Context) ([]wire.OutPoint, error) {

	readTxOpts := ReadTxOption()

	var result []wire.OutPoint

	err := s.db.ExecTx(ctx, readTxOpts, func(
		q SpendingReservationStore) error {

		rows, err := q.ListSpendingReservationOutpoints(ctx)
		if err != nil {
			return err
		}

		outpoints := make([]wire.OutPoint, 0, len(rows))
		for _, row := range rows {
			// NewHash validates the exact 32-byte length, so a
			// short or corrupt blob surfaces as an error rather
			// than a silently zero-padded outpoint.
			hash, err := chainhash.NewHash(row.OutpointHash)
			if err != nil {
				return err
			}

			outpoints = append(outpoints, wire.OutPoint{
				Hash:  *hash,
				Index: uint32(row.OutpointIndex),
			})
		}

		result = outpoints

		return nil
	})

	return result, err
}

// Compile-time check that the persistence store satisfies the VTXO manager's
// reservation interface. The OOR-side oor.ReservationStore is asserted at the
// wiring site (waved) instead, because db cannot import oor without an import
// cycle.
var _ vtxo.SpendingReservationStore = (*SpendingReservationPersistenceStore)(
	nil,
)
