package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// ownerKindOOROutgoing mirrors oor.ReservationOwnerKindOOROutgoing. It is
// duplicated here because the db package test cannot import oor (oor imports
// db, which would form a test import cycle).
const ownerKindOOROutgoing = 0

const ownerKindTaprootAssetPreparation = 1

// newSpendingReservationStoreForTest creates a spending-reservation store
// backed by a fresh test database.
func newSpendingReservationStoreForTest(t *testing.T) (
	*SpendingReservationPersistenceStore, *VTXOPersistenceStore,
	*RoundPersistenceStore) {

	t.Helper()

	vtxoStore, roundStore, baseDB := newVTXOStoreForTest(t)

	reservationDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) SpendingReservationStore {
			return baseDB.WithTx(tx)
		},
		btclog.Disabled,
	)

	reservationStore := NewSpendingReservationPersistenceStore(
		reservationDB, clock.NewDefaultClock(),
	)

	return reservationStore, vtxoStore, roundStore
}

// persistReservationTestVTXOs inserts one test VTXO per requested status and
// returns their outpoints in the same order.
func persistReservationTestVTXOs(t *testing.T, vtxoStore *VTXOPersistenceStore,
	roundStore *RoundPersistenceStore,
	statuses ...vtxo.VTXOStatus) []wire.OutPoint {

	t.Helper()

	ctx := t.Context()
	roundID := testRoundIDDB("spending-reservation-set")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	outpoints := make([]wire.OutPoint, 0, len(statuses))
	for i, status := range statuses {
		desc := createTestVTXODescriptor(t, roundID, 40+i)
		require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
		if status != vtxo.VTXOStatusLive {
			require.NoError(
				t, vtxoStore.UpdateVTXOStatus(
					ctx, desc.Outpoint, status,
				),
			)
		}

		outpoints = append(outpoints, desc.Outpoint)
	}

	return outpoints
}

// TestSpendingReservationStoreUpsertList verifies the upsert/list lifecycle of
// the durable spending-reservation index, including idempotent re-upsert. Row
// deletion is exercised through the VTXO store's atomic status-change path (see
// TestUpdateVTXOStatusReleasingReservation).
func TestSpendingReservationStoreUpsertList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, _ := newSpendingReservationStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 7}
	ownerID := chainhash.Hash{0x11, 0x22, 0x33}

	// An empty index lists nothing.
	got, err := store.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.Empty(t, got)

	// Upsert two reservations.
	require.NoError(
		t, store.UpsertReservation(
			ctx, opA, ownerKindOOROutgoing, ownerID,
		),
	)
	require.NoError(
		t, store.UpsertReservation(
			ctx, opB, ownerKindOOROutgoing, ownerID,
		),
	)

	got, err = store.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []wire.OutPoint{opA, opB}, got)

	// Re-upserting the same outpoint is idempotent: still two rows.
	newOwner := chainhash.Hash{0x44, 0x55}
	require.NoError(
		t, store.UpsertReservation(
			ctx, opA, ownerKindOOROutgoing, newOwner,
		),
	)

	got, err = store.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []wire.OutPoint{opA, opB}, got)
}

// TestSpendingReservationStoreUpsertSetAtomicConflict verifies that a batch
// conflict rolls back rows inserted earlier in the same transaction and does
// not hand an existing row from its original owner to the requested owner.
func TestSpendingReservationStoreUpsertSetAtomicConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)

	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	fresh := outpoints[0]
	conflict := outpoints[1]
	originalOwner := chainhash.Hash{0x11}
	requestedOwner := chainhash.Hash{0x22}

	require.NoError(
		t, store.UpsertReservation(
			ctx, conflict, ownerKindOOROutgoing, originalOwner,
		),
	)

	err := store.UpsertReservationSet(
		ctx, []wire.OutPoint{fresh, conflict},
		ownerKindTaprootAssetPreparation, requestedOwner,
	)
	require.ErrorContains(t, err, "different owner")

	reserved, err := store.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{conflict}, reserved)

	state, err := store.InspectReservationSet(
		ctx, []wire.OutPoint{conflict}, ownerKindOOROutgoing,
		originalOwner,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	state, err = store.InspectReservationSet(
		ctx, []wire.OutPoint{conflict},
		ownerKindTaprootAssetPreparation, requestedOwner,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetInconsistent, state)
}

// TestSpendingReservationStoreUpsertSet verifies that a completely absent set
// is acquired for one owner in a single operation and can be acquired again
// idempotently.
func TestSpendingReservationStoreUpsertSet(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)

	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	ownerID := chainhash.Hash{0x66}

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			ownerID,
		),
	)

	state, err := store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			ownerID,
		),
	)
}

// TestSpendingReservationStoreUpsertSetRejectsNonSpending verifies that every
// member must still be claimed by the wallet. A later Live or UnilateralExit
// member rejects the whole batch without leaving the earlier Spending member
// reserved.
func TestSpendingReservationStoreUpsertSetRejectsNonSpending(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status vtxo.VTXOStatus
	}{
		{
			name:   "live",
			status: vtxo.VTXOStatusLive,
		},
		{
			name:   "unilateral exit",
			status: vtxo.VTXOStatusUnilateralExit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			store, vtxoStore, roundStore :=
				newSpendingReservationStoreForTest(t)
			outpoints := persistReservationTestVTXOs(
				t, vtxoStore, roundStore,
				vtxo.VTXOStatusSpending, test.status,
			)

			err := store.UpsertReservationSet(
				ctx, outpoints,
				ownerKindTaprootAssetPreparation,
				chainhash.Hash{0x77},
			)
			require.ErrorContains(t, err, "must be spending")

			reserved, err := store.ListReservedOutpoints(ctx)
			require.NoError(t, err)
			require.Empty(t, reserved)
		})
	}
}

// TestSpendingReservationStoreUpsertSetRejectsMissingVTXO verifies an unknown
// outpoint cannot create a reservation row.
func TestSpendingReservationStoreUpsertSetRejectsMissingVTXO(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, _ := newSpendingReservationStoreForTest(t)
	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xee}, Index: 8}

	err := store.UpsertReservationSet(
		ctx, []wire.OutPoint{outpoint},
		ownerKindTaprootAssetPreparation, chainhash.Hash{0x88},
	)
	require.ErrorContains(t, err, "not found")

	reserved, err := store.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.Empty(t, reserved)
}

// TestSpendingReservationStoreHandoffSet verifies an exact-owner handoff and
// its fully already-to-owned replay.
func TestSpendingReservationStoreHandoffSet(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)
	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	fromID := chainhash.Hash{0x91}
	toID := chainhash.Hash{0x92}

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			fromID,
		),
	)
	require.NoError(
		t, store.HandoffReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			fromID, ownerKindOOROutgoing, toID,
		),
	)

	state, err := store.InspectReservationSet(
		ctx, outpoints, ownerKindOOROutgoing, toID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	// Replaying the same handoff sees the complete set already owned by
	// the destination and succeeds without stealing or inserting rows.
	require.NoError(
		t, store.HandoffReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			fromID, ownerKindOOROutgoing, toID,
		),
	)
}

// TestSpendingReservationStoreHandoffSetRejectsMissing verifies that neither
// a missing wallet row nor a missing reservation row can be created by a
// handoff. The latter also proves a prior row update rolls back atomically.
func TestSpendingReservationStoreHandoffSetRejectsMissing(t *testing.T) {
	t.Parallel()

	t.Run("VTXO", func(t *testing.T) {
		t.Parallel()

		ctx := t.Context()
		store, _, _ := newSpendingReservationStoreForTest(t)
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				0x93,
			},
			Index: 9,
		}

		err := store.HandoffReservationSet(
			ctx, []wire.OutPoint{outpoint},
			ownerKindTaprootAssetPreparation, chainhash.Hash{0x94},
			ownerKindOOROutgoing, chainhash.Hash{0x95},
		)
		require.ErrorContains(t, err, "VTXO")

		reserved, err := store.ListReservedOutpoints(ctx)
		require.NoError(t, err)
		require.Empty(t, reserved)
	})

	t.Run("reservation", func(t *testing.T) {
		t.Parallel()

		ctx := t.Context()
		store, vtxoStore, roundStore :=
			newSpendingReservationStoreForTest(t)
		outpoints := persistReservationTestVTXOs(
			t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
			vtxo.VTXOStatusSpending,
		)
		fromID := chainhash.Hash{0x96}
		toID := chainhash.Hash{0x97}

		require.NoError(
			t, store.UpsertReservationSet(
				ctx, outpoints[:1],
				ownerKindTaprootAssetPreparation, fromID,
			),
		)

		err := store.HandoffReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			fromID, ownerKindOOROutgoing, toID,
		)
		require.ErrorContains(t, err, "not found")

		state, err := store.InspectReservationSet(
			ctx, outpoints[:1], ownerKindTaprootAssetPreparation,
			fromID,
		)
		require.NoError(t, err)
		require.Equal(t, libtypes.ReservationSetOwned, state)
	})
}

// TestSpendingReservationStoreHandoffSetRejectsWrongOwner verifies a third
// owner on a later row rejects the handoff and rolls back the first row.
func TestSpendingReservationStoreHandoffSetRejectsWrongOwner(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)
	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	fromID := chainhash.Hash{0x98}
	otherID := chainhash.Hash{0x99}
	toID := chainhash.Hash{0x9a}

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints[:1], ownerKindTaprootAssetPreparation,
			fromID,
		),
	)
	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints[1:], ownerKindOOROutgoing, otherID,
		),
	)

	err := store.HandoffReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, fromID,
		ownerKindOOROutgoing, toID,
	)
	require.ErrorContains(t, err, "different owner")

	state, err := store.InspectReservationSet(
		ctx, outpoints[:1], ownerKindTaprootAssetPreparation, fromID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	state, err = store.InspectReservationSet(
		ctx, outpoints[1:], ownerKindOOROutgoing, otherID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)
}

// TestSpendingReservationStoreHandoffSetRejectsMixedOwners verifies a partial
// replay is not accepted: a from-owned row followed by a to-owned row fails
// and leaves both rows with their original owners.
func TestSpendingReservationStoreHandoffSetRejectsMixedOwners(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)
	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	fromID := chainhash.Hash{0x9b}
	toID := chainhash.Hash{0x9c}

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints[:1], ownerKindTaprootAssetPreparation,
			fromID,
		),
	)
	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints[1:], ownerKindOOROutgoing, toID,
		),
	)

	err := store.HandoffReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, fromID,
		ownerKindOOROutgoing, toID,
	)
	require.ErrorContains(t, err, "mixed reservation ownership")

	state, err := store.InspectReservationSet(
		ctx, outpoints[:1], ownerKindTaprootAssetPreparation, fromID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	state, err = store.InspectReservationSet(
		ctx, outpoints[1:], ownerKindOOROutgoing, toID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)
}

// TestSpendingReservationStoreHandoffSetRejectsNonSpending verifies a stale
// wallet-state member rejects the complete handoff before any owner row moves.
func TestSpendingReservationStoreHandoffSetRejectsNonSpending(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, vtxoStore, roundStore :=
		newSpendingReservationStoreForTest(t)
	outpoints := persistReservationTestVTXOs(
		t, vtxoStore, roundStore, vtxo.VTXOStatusSpending,
		vtxo.VTXOStatusSpending,
	)
	fromID := chainhash.Hash{0x9d}

	require.NoError(
		t, store.UpsertReservationSet(
			ctx, outpoints, ownerKindTaprootAssetPreparation,
			fromID,
		),
	)
	require.NoError(
		t, vtxoStore.UpdateVTXOStatus(
			ctx, outpoints[1], vtxo.VTXOStatusLive,
		),
	)

	err := store.HandoffReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, fromID,
		ownerKindOOROutgoing, chainhash.Hash{0x9e},
	)
	require.ErrorContains(t, err, "must be spending")

	state, err := store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, fromID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)
}

// TestSpendingReservationStoreHandoffSetRejectsInvalid verifies empty and
// duplicate handoff sets are rejected before database access.
func TestSpendingReservationStoreHandoffSetRejectsInvalid(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, _ := newSpendingReservationStoreForTest(t)
	fromID := chainhash.Hash{0x9f}
	toID := chainhash.Hash{0xa0}

	err := store.HandoffReservationSet(
		ctx, nil, ownerKindTaprootAssetPreparation, fromID,
		ownerKindOOROutgoing, toID,
	)
	require.ErrorContains(t, err, "must not be empty")

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xa1}, Index: 10}
	err = store.HandoffReservationSet(
		ctx, []wire.OutPoint{outpoint, outpoint},
		ownerKindTaprootAssetPreparation, fromID, ownerKindOOROutgoing,
		toID,
	)
	require.ErrorContains(t, err, "duplicate")
}

// TestSpendingReservationStoreInspectSet verifies absent, exact-owner, and
// inconsistent reservation sets, including both partial and wrong-owner
// shapes.
func TestSpendingReservationStoreInspectSet(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, _ := newSpendingReservationStoreForTest(t)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 3}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 4}
	outpoints := []wire.OutPoint{opA, opB}
	ownerID := chainhash.Hash{0x33}
	otherOwnerID := chainhash.Hash{0x44}

	state, err := store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetAbsent, state)

	require.NoError(
		t, store.UpsertReservation(
			ctx, opA, ownerKindTaprootAssetPreparation, ownerID,
		),
	)

	state, err = store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetInconsistent, state)

	require.NoError(
		t, store.UpsertReservation(
			ctx, opB, ownerKindTaprootAssetPreparation,
			otherOwnerID,
		),
	)

	state, err = store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetInconsistent, state)

	// The singular upsert intentionally retains owner-handoff semantics.
	require.NoError(
		t, store.UpsertReservation(
			ctx, opB, ownerKindTaprootAssetPreparation, ownerID,
		),
	)

	state, err = store.InspectReservationSet(
		ctx, outpoints, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetOwned, state)

	state, err = store.InspectReservationSet(
		ctx, outpoints, ownerKindOOROutgoing, ownerID,
	)
	require.NoError(t, err)
	require.Equal(t, libtypes.ReservationSetInconsistent, state)

}

// TestSpendingReservationStoreRejectsInvalidSet verifies that empty and
// duplicate request shapes fail before either the read or write transaction
// can observe an ambiguous set.
func TestSpendingReservationStoreRejectsInvalidSet(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, _ := newSpendingReservationStoreForTest(t)
	ownerID := chainhash.Hash{0x55}
	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xcc}, Index: 5}

	err := store.UpsertReservationSet(
		ctx, nil, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.ErrorContains(t, err, "must not be empty")

	_, err = store.InspectReservationSet(
		ctx, nil, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.ErrorContains(t, err, "must not be empty")

	duplicates := []wire.OutPoint{outpoint, outpoint}
	err = store.UpsertReservationSet(
		ctx, duplicates, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.ErrorContains(t, err, "duplicate")

	_, err = store.InspectReservationSet(
		ctx, duplicates, ownerKindTaprootAssetPreparation, ownerID,
	)
	require.ErrorContains(t, err, "duplicate")
}
