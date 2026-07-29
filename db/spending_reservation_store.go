package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
)

// SpendingReservationStore groups the SQL methods needed to maintain the
// durable spending-reservation index.
type SpendingReservationStore interface {
	GetVTXO(ctx context.Context, arg sqlc.GetVTXOParams) (sqlc.Vtxo, error)

	GetSpendingReservation(ctx context.Context,
		arg sqlc.GetSpendingReservationParams) (
		sqlc.GetSpendingReservationRow, error)

	HandoffSpendingReservation(ctx context.Context,
		arg sqlc.HandoffSpendingReservationParams) (int64, error)

	InsertSpendingReservation(ctx context.Context,
		arg sqlc.InsertSpendingReservationParams) (int64, error)

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
// outpoints reserved by an active spend owner, such as a Taproot Asset
// preparation or outgoing OOR session. A row exists only after the owner has
// crossed its durable handoff boundary, so a startup sweep can
// deterministically release orphaned Spending VTXOs that have no reservation
// row.
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

// UpsertReservationSet atomically reserves every outpoint for one exact
// owner. Existing rows owned by that owner are idempotent; a row owned by
// anyone else aborts and rolls back the complete set. The singular
// UpsertReservation method deliberately retains its owner-handoff behavior.
func (s *SpendingReservationPersistenceStore) UpsertReservationSet(
	ctx context.Context, outpoints []wire.OutPoint, ownerKind int,
	ownerID chainhash.Hash,
) error {

	err := validateReservationSetOutpoints(outpoints)
	if err != nil {
		return err
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q SpendingReservationStore) error {

		// Validate the complete wallet-state precondition before
		// reading or inserting any reservation rows. Keeping this check
		// in the same write transaction closes the gap where a
		// reservation could be acquired for a coin the wallet no longer
		// considers claimed.
		for _, outpoint := range outpoints {
			row, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			})
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("reservation VTXO %v not "+
					"found: %w", outpoint, err)
			}
			if err != nil {
				return err
			}

			if vtxo.VTXOStatus(row.Status) !=
				vtxo.VTXOStatusSpending {
				return fmt.Errorf("reservation VTXO %v must "+
					"be spending, got status %d", outpoint,
					row.Status)
			}
		}

		createdAt := s.clock.Now().Unix()
		for _, outpoint := range outpoints {
			getParams := sqlc.GetSpendingReservationParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			}

			row, err := q.GetSpendingReservation(ctx, getParams)
			switch {
			case err == nil:
				if reservationOwnerMatches(
					row.OwnerKind, row.OwnerID, ownerKind,
					ownerID,
				) {

					continue
				}

				return fmt.Errorf("spending reservation %v "+
					"belongs to a different owner",
					outpoint)

			case errors.Is(err, sql.ErrNoRows):
				// The row is acquired below in the same
				// database transaction as every other member of
				// the set.

			default:
				return err
			}

			params := sqlc.InsertSpendingReservationParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
				OwnerKind:     int32(ownerKind),
				OwnerID:       ownerID[:],
				CreatedAt:     createdAt,
			}

			rows, err := q.InsertSpendingReservation(ctx, params)
			if err != nil {
				return err
			}

			if rows == 1 {
				continue
			}

			// A concurrent transaction acquired the absent row
			// before this insert. Resolve the winner without
			// invoking the singular upsert's intentional
			// owner-handoff behavior.
			row, err = q.GetSpendingReservation(ctx, getParams)
			if err != nil {
				return err
			}

			if !reservationOwnerMatches(
				row.OwnerKind, row.OwnerID, ownerKind, ownerID,
			) {
				return fmt.Errorf("spending reservation %v "+
					"belongs to a different owner",
					outpoint)
			}
		}

		return nil
	})
}

// InspectReservationSet atomically classifies a complete outpoint set for one
// exact owner. A partial set, a mixed-owner set, or a fully reserved set owned
// by someone else is inconsistent rather than absent.
func (s *SpendingReservationPersistenceStore) InspectReservationSet(
	ctx context.Context, outpoints []wire.OutPoint, ownerKind int,
	ownerID chainhash.Hash) (libtypes.ReservationSetState, error) {

	err := validateReservationSetOutpoints(outpoints)
	if err != nil {
		return libtypes.ReservationSetInconsistent, err
	}

	state := libtypes.ReservationSetAbsent
	err = s.db.ExecTx(ctx, ReadTxOption(), func(
		q SpendingReservationStore) error {

		var (
			present int
			owned   int
		)
		for _, outpoint := range outpoints {
			row, err := q.GetSpendingReservation(
				ctx, sqlc.GetSpendingReservationParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
				},
			)
			switch {
			case err == nil:
				present++
				if reservationOwnerMatches(
					row.OwnerKind, row.OwnerID, ownerKind,
					ownerID,
				) {

					owned++
				}

			case errors.Is(err, sql.ErrNoRows):
				continue

			default:
				return err
			}
		}

		switch {
		case present == 0:
			state = libtypes.ReservationSetAbsent

		case present == len(outpoints) && owned == len(outpoints):
			state = libtypes.ReservationSetOwned

		default:
			state = libtypes.ReservationSetInconsistent
		}

		return nil
	})

	return state, err
}

// HandoffReservationSet atomically transfers a complete reservation set from
// one exact owner to another. It never creates rows and never takes rows from
// a third owner. A fully already-to-owned set is accepted as an idempotent
// replay, while any mixture of from/to ownership fails closed.
func (s *SpendingReservationPersistenceStore) HandoffReservationSet(
	ctx context.Context, outpoints []wire.OutPoint, fromKind int,
	fromID chainhash.Hash, toKind int, toID chainhash.Hash) error {

	err := validateReservationSetOutpoints(outpoints)
	if err != nil {
		return err
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q SpendingReservationStore) error {

		// Validate the complete wallet-state precondition before
		// reading or changing reservation ownership. If any member has
		// left Spending, no owner row may move.
		for _, outpoint := range outpoints {
			row, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			})
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("reservation VTXO %v not "+
					"found: %w", outpoint, err)
			}
			if err != nil {
				return err
			}

			if vtxo.VTXOStatus(row.Status) !=
				vtxo.VTXOStatusSpending {
				return fmt.Errorf("reservation VTXO %v must "+
					"be spending, got status %d", outpoint,
					row.Status)
			}
		}

		var replay bool
		for i, outpoint := range outpoints {
			getParams := sqlc.GetSpendingReservationParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			}

			row, err := q.GetSpendingReservation(ctx, getParams)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("spending reservation %v "+
					"not found: %w", outpoint, err)
			}
			if err != nil {
				return err
			}

			fromOwned := reservationOwnerMatches(
				row.OwnerKind, row.OwnerID, fromKind, fromID,
			)
			toOwned := reservationOwnerMatches(
				row.OwnerKind, row.OwnerID, toKind, toID,
			)

			if i == 0 {
				switch {
				case fromOwned:
					replay = false

				case toOwned:
					replay = true

				default:
					return fmt.Errorf("spending "+
						"reservation %v belongs to a "+
						"different owner", outpoint)
				}
			}

			expectedKind := fromKind
			expectedID := fromID
			if replay {
				expectedKind = toKind
				expectedID = toID
			}

			if !reservationOwnerMatches(
				row.OwnerKind, row.OwnerID, expectedKind,
				expectedID,
			) {

				if fromOwned || toOwned {
					return fmt.Errorf("mixed reservation "+
						"ownership at %v", outpoint)
				}

				return fmt.Errorf("spending reservation %v "+
					"belongs to a different owner",
					outpoint)
			}

			rows, err := q.HandoffSpendingReservation(
				ctx, sqlc.HandoffSpendingReservationParams{
					ToOwnerKind:   int32(toKind),
					ToOwnerID:     toID[:],
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
					FromOwnerKind: int32(expectedKind),
					FromOwnerID:   expectedID[:],
				},
			)
			if err != nil {
				return err
			}

			if rows != 1 {
				return fmt.Errorf("spending reservation %v "+
					"owner changed concurrently", outpoint)
			}
		}

		return nil
	})
}

// validateReservationSetOutpoints rejects request shapes whose set semantics
// would otherwise be ambiguous.
func validateReservationSetOutpoints(outpoints []wire.OutPoint) error {
	if len(outpoints) == 0 {
		return fmt.Errorf("reservation outpoints must not be empty")
	}

	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		if _, ok := seen[outpoint]; ok {
			return fmt.Errorf("duplicate reservation outpoint %v",
				outpoint)
		}

		seen[outpoint] = struct{}{}
	}

	return nil
}

// reservationOwnerMatches reports whether a stored owner is byte-for-byte
// identical to the requested owner.
func reservationOwnerMatches(storedKind int32, storedID []byte, ownerKind int,
	ownerID chainhash.Hash) bool {

	return storedKind == int32(ownerKind) && bytes.Equal(
		storedID, ownerID[:],
	)
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
