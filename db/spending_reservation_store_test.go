package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// ownerKindOOROutgoing mirrors oor.ReservationOwnerKindOOROutgoing. It is
// duplicated here because the db package test cannot import oor (oor imports
// db, which would form a test import cycle).
const ownerKindOOROutgoing = 0

// newSpendingReservationStoreForTest creates a spending-reservation store
// backed by a fresh test database.
func newSpendingReservationStoreForTest(
	t *testing.T) *SpendingReservationPersistenceStore {

	t.Helper()

	db := NewTestDB(t)

	reservationDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) SpendingReservationStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewSpendingReservationPersistenceStore(
		reservationDB, clock.NewDefaultClock(),
	)
}

// TestSpendingReservationStoreUpsertList verifies the upsert/list lifecycle of
// the durable spending-reservation index, including idempotent re-upsert. Row
// deletion is exercised through the VTXO store's atomic status-change path (see
// TestUpdateVTXOStatusReleasingReservation).
func TestSpendingReservationStoreUpsertList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newSpendingReservationStoreForTest(t)

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
