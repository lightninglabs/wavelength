package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newBatchCanonicalityStoreForTest creates a batch canonicality store backed
// by a fresh test database.
func newBatchCanonicalityStoreForTest(
	t *testing.T) *BatchCanonicalityPersistenceStore {

	t.Helper()

	db := NewTestDB(t)

	canonDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) BatchCanonicalityStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewBatchCanonicalityPersistenceStore(
		canonDB, clock.NewDefaultClock(),
	)
}

// outpoint is a small test helper building a deterministic outpoint.
func outpoint(b byte, index uint32) wire.OutPoint {
	return wire.OutPoint{Hash: chainhash.Hash{b}, Index: index}
}

// consumedInput builds a batchcanon.ConsumedInput from an outpoint with a
// deterministic non-empty pkScript so persistence round-trips can assert the
// script is stored and reloaded alongside the outpoint.
func consumedInput(op wire.OutPoint) batchcanon.ConsumedInput {
	return batchcanon.ConsumedInput{
		Outpoint: op,
		PkScript: []byte{
			0x51,
			0x20,
			op.Hash[0],
		},
	}
}

// TestBatchCanonicalityUpsertRoundTrip verifies a record survives an upsert
// and read with all of its fields, consumed inputs, and dependent VTXOs.
func TestBatchCanonicalityUpsertRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xaa}
	rec := &batchcanon.Record{
		BatchTxID:          txid,
		State:              batchcanon.StateProvisional,
		ConfirmationHeight: fn.Some[int32](100),
		ConfirmationBlock:  fn.Some(chainhash.Hash{0xbb}),
		CSVExpiryDelta:     144,
		PolicyState:        batchcanon.PolicyStateDefault,
		ConsumedInputs: []batchcanon.ConsumedInput{
			consumedInput(outpoint(0x01, 0)),
			consumedInput(outpoint(0x02, 3)),
		},
		DependentVTXOs: []wire.OutPoint{
			outpoint(0x03, 1),
		},
	}
	require.NoError(t, store.UpsertBatch(ctx, rec))

	got, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(t, txid, got.BatchTxID)
	require.Equal(t, batchcanon.StateProvisional, got.State)
	require.Equal(t, int32(100), got.ConfirmationHeight.UnwrapOr(0))
	require.True(t, got.ConfirmationBlock.IsSome())
	require.Equal(t, int32(144), got.CSVExpiryDelta)
	require.Equal(t, batchcanon.PolicyStateDefault, got.PolicyState)
	require.ElementsMatch(t, rec.ConsumedInputs, got.ConsumedInputs)
	require.ElementsMatch(t, rec.DependentVTXOs, got.DependentVTXOs)

	// Effective expiry derives from the stored confirmation.
	require.Equal(t, int32(244), got.EffectiveExpiry().UnwrapOr(0))
}

// TestBatchCanonicalityRecordInputConflict verifies that per-input conflict
// flags round-trip through the store, so restart reconciliation can rebuild
// the per-input conflict view (darepo#454 reconciliation-ordering fix).
func TestBatchCanonicalityRecordInputConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xac}
	inA := outpoint(0x01, 0)
	inB := outpoint(0x02, 1)
	rec := &batchcanon.Record{
		BatchTxID:      txid,
		State:          batchcanon.StateConflictProvisional,
		CSVExpiryDelta: 144,
		ConsumedInputs: []batchcanon.ConsumedInput{
			consumedInput(inA),
			consumedInput(inB),
		},
	}
	require.NoError(t, store.UpsertBatch(ctx, rec))

	// Freshly inserted inputs carry no conflict.
	got, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	for _, in := range got.ConsumedInputs {
		require.False(t, in.Conflicting)
		require.False(t, in.ConflictFinal)
	}

	// Mark input A conflicting (provisional), leave B untouched.
	require.NoError(
		t, store.RecordInputConflict(ctx, txid, inA, true, false),
	)
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, inputFlags(t, got, inA).Conflicting)
	require.False(t, inputFlags(t, got, inA).ConflictFinal)
	require.False(t, inputFlags(t, got, inB).Conflicting)

	// Promote input A to a finalized conflict; the flag persists.
	require.NoError(
		t, store.RecordInputConflict(ctx, txid, inA, true, true),
	)
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, inputFlags(t, got, inA).Conflicting)
	require.True(t, inputFlags(t, got, inA).ConflictFinal)

	// Clearing the conflict (spend reorged away) resets both flags.
	require.NoError(
		t, store.RecordInputConflict(ctx, txid, inA, false, false),
	)
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.False(t, inputFlags(t, got, inA).Conflicting)
	require.False(t, inputFlags(t, got, inA).ConflictFinal)
}

// inputFlags returns the consumed input matching op from a record, failing the
// test if it is absent.
func inputFlags(t *testing.T, rec *batchcanon.Record,
	op wire.OutPoint) batchcanon.ConsumedInput {

	t.Helper()
	for _, in := range rec.ConsumedInputs {
		if in.Outpoint == op {
			return in
		}
	}
	t.Fatalf("consumed input %v not found", op)

	return batchcanon.ConsumedInput{}
}

// TestBatchCanonicalityGetNotFound verifies the not-found sentinel.
func TestBatchCanonicalityGetNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	_, err := store.GetBatch(ctx, chainhash.Hash{0xff})
	require.ErrorIs(t, err, batchcanon.ErrBatchNotFound)
}

// TestBatchCanonicalityUpsertReplacesEdges verifies a re-upsert replaces the
// consumed-input and dependent-VTXO sets rather than appending.
func TestBatchCanonicalityUpsertReplacesEdges(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xa1}
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      txid,
				State:          batchcanon.StateUnseen,
				CSVExpiryDelta: 10,
				ConsumedInputs: []batchcanon.ConsumedInput{
					consumedInput(outpoint(0x01, 0)),
				},
				DependentVTXOs: []wire.OutPoint{
					outpoint(0x02, 0),
				},
			},
		),
	)

	// Re-upsert with a different edge set.
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      txid,
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 10,
				ConsumedInputs: []batchcanon.ConsumedInput{
					consumedInput(outpoint(0x09, 2)),
				},
				DependentVTXOs: nil,
			},
		),
	)

	got, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(
		t, []batchcanon.ConsumedInput{consumedInput(outpoint(0x09, 2))},
		got.ConsumedInputs,
	)
	require.Empty(t, got.DependentVTXOs)
}

// TestBatchCanonicalityReorgRecomputesExpiry verifies the reorg-aware expiry
// contract end to end through the store: a confirmation yields an effective
// expiry, a reorg (ClearConfirmation) erases it, and a reconfirmation at a new
// height yields a fresh effective expiry. Expiry is never frozen.
func TestBatchCanonicalityReorgRecomputesExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xc0}
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      txid,
				State:          batchcanon.StateUnseen,
				CSVExpiryDelta: 144,
			},
		),
	)

	// Unconfirmed: no effective expiry.
	got, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, got.EffectiveExpiry().IsNone())

	// Confirm at height 100.
	require.NoError(
		t,
		store.RecordConfirmation(
			ctx, txid, 100, chainhash.Hash{0xc1},
		),
	)
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(t, int32(244), got.EffectiveExpiry().UnwrapOr(0))

	// Reorg out: confirmation cleared, effective expiry erased.
	require.NoError(t, store.ClearConfirmation(ctx, txid))
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, got.ConfirmationHeight.IsNone())
	require.True(t, got.EffectiveExpiry().IsNone())

	// Reconfirm at a higher height: fresh effective expiry.
	require.NoError(
		t,
		store.RecordConfirmation(
			ctx, txid, 105, chainhash.Hash{0xc2},
		),
	)
	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(t, int32(249), got.EffectiveExpiry().UnwrapOr(0))
}

// TestBatchCanonicalityStateNotTerminal verifies state can move freely in any
// direction (finalized -> reorged_out -> provisional), proving no state is
// persisted as an irreversible terminal verdict.
func TestBatchCanonicalityStateNotTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xd0}
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      txid,
				State:          batchcanon.StateFinalized,
				CSVExpiryDelta: 10,
			},
		),
	)

	for _, want := range []batchcanon.State{
		batchcanon.StateReorgedOut,
		batchcanon.StateConflictFinalized,
		batchcanon.StateProvisional,
	} {
		require.NoError(t, store.UpdateBatchState(ctx, txid, want))
		got, err := store.GetBatch(ctx, txid)
		require.NoError(t, err)
		require.Equal(t, want, got.State)
	}
}

// TestBatchCanonicalityListByState verifies state-filtered listing.
func TestBatchCanonicalityListByState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      chainhash.Hash{0xe0},
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 1,
			},
		),
	)
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      chainhash.Hash{0xe1},
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 1,
			},
		),
	)
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      chainhash.Hash{0xe2},
				State:          batchcanon.StateFinalized,
				CSVExpiryDelta: 1,
			},
		),
	)

	prov, err := store.ListBatchesByState(ctx, batchcanon.StateProvisional)
	require.NoError(t, err)
	require.Len(t, prov, 2)

	final, err := store.ListBatchesByState(ctx, batchcanon.StateFinalized)
	require.NoError(t, err)
	require.Len(t, final, 1)
}

// TestBatchCanonicalityFindByConsumedOutpoint verifies input-conflict
// detection: two batches consuming the same outpoint are both found.
func TestBatchCanonicalityFindByConsumedOutpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	shared := outpoint(0x55, 1)
	batchA := chainhash.Hash{0xa0}
	batchB := chainhash.Hash{0xb0}

	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      batchA,
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 1,
				ConsumedInputs: []batchcanon.ConsumedInput{
					consumedInput(shared),
				},
			},
		),
	)
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      batchB,
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 1,
				ConsumedInputs: []batchcanon.ConsumedInput{
					consumedInput(shared),
				},
			},
		),
	)

	found, err := store.FindBatchesConsumingOutpoint(ctx, shared)
	require.NoError(t, err)
	require.ElementsMatch(t, []chainhash.Hash{batchA, batchB}, found)

	none, err := store.FindBatchesConsumingOutpoint(ctx, outpoint(0x99, 0))
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestBatchCanonicalityProvisionalConsumerRestore verifies the reverse-
// dependency lifecycle: a provisionally consumed VTXO is listed for its
// consumer batch (so it can be restored if the batch is invalidated), survives
// a batch state change, and is removed on delete.
func TestBatchCanonicalityProvisionalConsumerRestore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	consumerBatch := chainhash.Hash{0xf0}
	require.NoError(
		t,
		store.UpsertBatch(
			ctx, &batchcanon.Record{
				BatchTxID:      consumerBatch,
				State:          batchcanon.StateProvisional,
				CSVExpiryDelta: 1,
			},
		),
	)

	consumed := outpoint(0x44, 2)
	require.NoError(
		t, store.AddProvisionalConsumer(
			ctx, consumed, consumerBatch,
		),
	)

	// Listed for the consumer batch.
	got, err := store.ListProvisionalConsumersForBatch(ctx, consumerBatch)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{consumed}, got)

	// Idempotent re-add.
	require.NoError(
		t, store.AddProvisionalConsumer(
			ctx, consumed, consumerBatch,
		),
	)
	got, err = store.ListProvisionalConsumersForBatch(ctx, consumerBatch)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// The edge survives the batch being marked reorged/invalidated — that
	// is exactly when the restore caller needs to read it.
	require.NoError(
		t, store.UpdateBatchState(
			ctx, consumerBatch, batchcanon.StateReorgedOut,
		),
	)
	got, err = store.ListProvisionalConsumersForBatch(ctx, consumerBatch)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{consumed}, got)

	// Deleting clears the edges (e.g. once the consumption is canonical or
	// fully reconciled).
	require.NoError(
		t, store.DeleteProvisionalConsumersForBatch(
			ctx, consumerBatch,
		),
	)
	got, err = store.ListProvisionalConsumersForBatch(ctx, consumerBatch)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestBatchCanonicalityBackfillFromVTXOs verifies that backfill derives one
// canonicality record per distinct batch present in the VTXO store, with the
// CSV-relative expiry delta recovered from the stored absolute batch_expiry,
// the right provisional/finalized classification, and the dependent VTXO
// linked. It also verifies idempotency: a re-run creates nothing and does not
// clobber state the manager has since advanced.
func TestBatchCanonicalityBackfillFromVTXOs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	vtxoStore, roundStore, baseDB := newVTXOStoreForTest(t)

	canonDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) BatchCanonicalityStore {
			return baseDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	canon := NewBatchCanonicalityPersistenceStore(
		canonDB, clock.NewDefaultClock(),
	)

	// A round must exist to satisfy the VTXO foreign key.
	roundID := testRoundIDDB("backfill-round")
	testRound := createTestRound(t, roundID)
	sigState := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, sigState))

	// Two VTXOs in two distinct batches:
	//   idx 0: batch_expiry 1000, created_height 500
	//   idx 1: batch_expiry 1100, created_height 510
	desc0 := createTestVTXODescriptor(t, roundID, 0)
	desc1 := createTestVTXODescriptor(t, roundID, 1)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc0))
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc1))

	// best height 505, finality depth 6:
	//   batch 0: depth = 505-500+1 = 6 >= 6 -> finalized
	//   batch 1: depth = 505-510+1 < 6     -> provisional
	n, err := canon.BackfillFromVTXOs(ctx, 505, 6)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	rec0, err := canon.GetBatch(ctx, desc0.CommitmentTxID)
	require.NoError(t, err)
	require.Equal(t, batchcanon.StateFinalized, rec0.State)
	require.Equal(t, int32(500), rec0.ConfirmationHeight.UnwrapOr(0))
	require.Equal(t, int32(500), rec0.CSVExpiryDelta)
	require.Equal(t, int32(1000), rec0.EffectiveExpiry().UnwrapOr(0))
	require.Equal(t, []wire.OutPoint{desc0.Outpoint}, rec0.DependentVTXOs)

	rec1, err := canon.GetBatch(ctx, desc1.CommitmentTxID)
	require.NoError(t, err)
	require.Equal(t, batchcanon.StateProvisional, rec1.State)
	require.Equal(t, int32(590), rec1.CSVExpiryDelta)

	// Idempotency: advance one batch's state, re-run backfill, and verify
	// it creates nothing new and leaves the advanced state untouched.
	require.NoError(
		t, canon.UpdateBatchState(
			ctx, desc0.CommitmentTxID, batchcanon.StateReorgedOut,
		),
	)
	n, err = canon.BackfillFromVTXOs(ctx, 505, 6)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	rec0, err = canon.GetBatch(ctx, desc0.CommitmentTxID)
	require.NoError(t, err)
	require.Equal(t, batchcanon.StateReorgedOut, rec0.State)
}
