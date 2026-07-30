package db

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
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
		Value:    1_000 + int64(op.Index),
		PkScript: []byte{
			0x51,
			0x20,
			op.Hash[0],
		},
	}
}

func consumerEdge(op wire.OutPoint, revision uint64,
	lineage ...chainhash.Hash) batchcanon.ConsumerEdge {

	return batchcanon.ConsumerEdge{
		ConsumedVTXO:     op,
		ExpectedRevision: revision,
		CreatorLineage:   lineage,
	}
}

// readyBatchRecord builds a complete generation-one record for restore tests.
func readyBatchRecord(txid chainhash.Hash,
	state batchcanon.State) *batchcanon.Record {

	return &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			0x00,
		},
		ConfirmationPkScript: []byte{
			0x51,
		},
		ConsumedInputs: []batchcanon.ConsumedInput{
			consumedInput(wire.OutPoint{Hash: txid}),
		},
		RegistrationStage:     batchcanon.RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some[uint64](1),
		State:                 state,
	}
}

// consumerRestoreHarness owns stores and evidence for one forfeited VTXO.
type consumerRestoreHarness struct {
	canon        *BatchCanonicalityPersistenceStore
	vtxos        *VTXOPersistenceStore
	db           *BaseDB
	edge         batchcanon.ConsumerEdge
	consumer     chainhash.Hash
	forfeitTxID  chainhash.Hash
	creatorBatch chainhash.Hash
}

// newConsumerRestoreHarness creates one VTXO with an exact durable
// ForfeitedBy marker and a ready, usable creator lineage. The caller chooses
// when and how to register the consumer edge.
func newConsumerRestoreHarness(t *testing.T) *consumerRestoreHarness {
	t.Helper()

	ctx := t.Context()
	vtxos, rounds, baseDB := newVTXOStoreForTest(t)
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

	roundID := testRoundIDDB("consumer-restore-round")
	r := createTestRound(t, roundID)
	sigState := &round.InputSigSentState{
		RoundID:     r.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, rounds.CommitState(ctx, r, sigState))

	desc := createTestVTXODescriptor(t, roundID, 7)
	require.NoError(t, vtxos.SaveVTXO(ctx, desc))
	require.NoError(
		t,
		vtxos.MarkForfeiting(
			ctx, desc.Outpoint, roundID.String(), nil,
		),
	)

	consumer := chainhash.Hash{0xb7}
	forfeitTxID := chainhash.Hash{0xf7}
	require.NoError(
		t, vtxos.MarkForfeited(
			ctx, desc.Outpoint, forfeitTxID, consumer,
		),
	)

	forfeited, err := vtxos.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeited, forfeited.Status)
	require.Equal(t, uint64(2), forfeited.BusinessRevision)
	require.Equal(
		t, consumer,
		forfeited.ForfeitConsumerBatch.UnwrapOr(
			chainhash.Hash{},
		),
	)

	creatorBatch := desc.CommitmentTxID
	require.NoError(
		t,
		canon.RegisterBatch(
			ctx, readyBatchRecord(
				creatorBatch, batchcanon.StateProvisional,
			),
			nil,
		),
	)

	return &consumerRestoreHarness{
		canon:        canon,
		vtxos:        vtxos,
		db:           baseDB,
		consumer:     consumer,
		forfeitTxID:  forfeitTxID,
		creatorBatch: creatorBatch,
		edge: batchcanon.ConsumerEdge{
			ConsumedVTXO:     desc.Outpoint,
			ConsumerBatch:    consumer,
			ExpectedRevision: forfeited.BusinessRevision,
			CreatorLineage: []chainhash.Hash{
				creatorBatch,
			},
		},
	}
}

// registerConsumer registers the harness consumer batch and exact edge.
func (h *consumerRestoreHarness) registerConsumer(t *testing.T,
	state batchcanon.State) {

	t.Helper()

	require.NoError(
		t,
		h.canon.RegisterBatch(
			t.Context(),
			readyBatchRecord(h.edge.ConsumerBatch, state),
			[]batchcanon.ConsumerEdge{h.edge},
		),
	)
}

// TestListPendingConsumerBatchesByCreator proves a creator-state change can
// target exactly the durable terminal-consumer checkpoints it may unblock.
func TestListPendingConsumerBatchesByCreator(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.registerConsumer(t, batchcanon.StateConflictFinalized)

	consumers, err := h.canon.ListPendingConsumerBatchesByCreator(
		t.Context(), h.creatorBatch,
	)
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{h.consumer}, consumers)

	consumers, err = h.canon.ListPendingConsumerBatchesByCreator(
		t.Context(), chainhash.Hash{0xee},
	)
	require.NoError(t, err)
	require.Empty(t, consumers)
}

// TestBatchCanonicalityUpsertRoundTrip verifies a record survives an upsert
// and read with all of its fields, consumed inputs, and dependent VTXOs.
func TestBatchCanonicalityUpsertRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)

	txid := chainhash.Hash{0xaa}
	rec := &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			0x02,
			0xaa,
			0xbb,
		},
		BatchOutputIndex:   1,
		State:              batchcanon.StateProvisional,
		ConfirmationHeight: fn.Some[int32](100),
		ConfirmationBlock:  fn.Some(chainhash.Hash{0xbb}),
		CSVExpiryDelta:     144,
		PolicyState:        batchcanon.PolicyStateDefault,
		WatchHeightHint:    77,
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
	require.Equal(t, rec.BatchTx, got.BatchTx)
	require.Equal(t, rec.BatchOutputIndex, got.BatchOutputIndex)
	require.Equal(t, batchcanon.StateProvisional, got.State)
	require.Equal(t, int32(100), got.ConfirmationHeight.UnwrapOr(0))
	require.True(t, got.ConfirmationBlock.IsSome())
	require.Equal(t, int32(144), got.CSVExpiryDelta)
	require.Equal(t, batchcanon.PolicyStateDefault, got.PolicyState)
	require.Equal(t, uint32(77), got.WatchHeightHint)
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
	consumed := outpoint(0x44, 2)
	record := &batchcanon.Record{
		BatchTxID:      consumerBatch,
		State:          batchcanon.StateProvisional,
		CSVExpiryDelta: 1,
	}
	edge := consumerEdge(
		consumed, 3, chainhash.Hash{0xe1}, chainhash.Hash{0xe2},
	)
	require.NoError(
		t,
		store.RegisterBatch(
			ctx, record, []batchcanon.ConsumerEdge{edge},
		),
	)

	// Listed for the consumer batch.
	got, err := store.ListPendingConsumerEdges(ctx, consumerBatch)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, consumed, got[0].ConsumedVTXO)
	require.Equal(t, uint64(3), got[0].ExpectedRevision)
	require.ElementsMatch(t, edge.CreatorLineage, got[0].CreatorLineage)

	// Idempotent re-add.
	require.NoError(
		t,
		store.RegisterBatch(
			ctx, record, []batchcanon.ConsumerEdge{edge},
		),
	)
	got, err = store.ListPendingConsumerEdges(ctx, consumerBatch)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// The edge survives the batch being marked reorged/invalidated — that
	// is exactly when the restore caller needs to read it.
	require.NoError(
		t, store.UpdateBatchState(
			ctx, consumerBatch, batchcanon.StateReorgedOut,
		),
	)
	got, err = store.ListPendingConsumerEdges(ctx, consumerBatch)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// Deleting clears the edges (e.g. once the consumption is canonical or
	// fully reconciled).
	require.NoError(
		t, store.DeleteProvisionalConsumersForBatch(
			ctx, consumerBatch,
		),
	)
	got, err = store.ListPendingConsumerEdges(ctx, consumerBatch)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestBatchRegistrationIsAtomicAndImmutable proves repeat registration can
// only merge monotonic edges. It cannot replace actual inputs or clear a
// persisted conflict, and contradictory evidence durably quarantines the
// record.
func TestBatchRegistrationIsAtomicAndImmutable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)
	txid := chainhash.Hash{0xf4}
	input := consumedInput(outpoint(0x11, 0))
	firstDependent := outpoint(0x12, 0)
	secondDependent := outpoint(0x13, 0)
	firstConsumer := outpoint(0x14, 0)
	secondConsumer := outpoint(0x15, 0)
	creator := chainhash.Hash{0xe3}

	record := &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			0x02,
			0xf4,
		},
		BatchOutputIndex:      1,
		RegistrationStage:     batchcanon.RegistrationRegistering,
		ObservationGeneration: 1,
		State:                 batchcanon.StateUnseen,
		CSVExpiryDelta:        144,
		WatchHeightHint:       90,
		ConfirmationPkScript: []byte{
			0x51,
			0x20,
			0xf4,
		},
		ConsumedInputs: []batchcanon.ConsumedInput{
			input,
		},
		DependentVTXOs: []wire.OutPoint{
			firstDependent,
		},
	}
	require.NoError(
		t,
		store.RegisterBatch(
			ctx, record, []batchcanon.ConsumerEdge{
				consumerEdge(firstConsumer, 2, creator),
			},
		),
	)
	require.NoError(
		t, store.RecordInputConflict(
			ctx, txid, input.Outpoint, true, false,
		),
	)

	// An idempotent retry may add dependents and consumer edges, but it
	// carries the same immutable output/input evidence.
	retry := *record
	// A reconfirmed indexer observation can report a later height. The
	// original lower scan point remains conservative and must not turn this
	// otherwise-identical registration into contradictory evidence.
	retry.WatchHeightHint = 105
	retry.DependentVTXOs = []wire.OutPoint{secondDependent}
	require.NoError(
		t,
		store.RegisterBatch(
			ctx, &retry, []batchcanon.ConsumerEdge{
				consumerEdge(secondConsumer, 4, creator),
			},
		),
	)

	got, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, inputFlags(t, got, input.Outpoint).Conflicting)
	require.Equal(t, uint32(90), got.WatchHeightHint)
	require.ElementsMatch(
		t, []wire.OutPoint{firstDependent, secondDependent},
		got.DependentVTXOs,
	)
	consumers, err := store.ListPendingConsumerEdges(ctx, txid)
	require.NoError(t, err)
	require.ElementsMatch(
		t, []wire.OutPoint{firstConsumer, secondConsumer},
		[]wire.OutPoint{
			consumers[0].ConsumedVTXO,
			consumers[1].ConsumedVTXO,
		},
	)

	// Removing/replacing an actual input is contradictory evidence. The
	// original set and conflict survive, while readiness is quarantined.
	conflict := retry
	conflict.ConsumedInputs = []batchcanon.ConsumedInput{
		consumedInput(outpoint(0x99, 0)),
	}
	err = store.RegisterBatch(ctx, &conflict, nil)
	require.ErrorIs(t, err, batchcanon.ErrRegistrationConflict)

	got, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(
		t, batchcanon.RegistrationQuarantined, got.RegistrationStage,
	)
	require.False(t, got.Ready())
	require.Equal(t, []batchcanon.ConsumedInput{
		{
			Outpoint:    input.Outpoint,
			Value:       input.Value,
			PkScript:    input.PkScript,
			Conflicting: true,
		},
	}, got.ConsumedInputs)
}

// TestBatchReadinessGeneration proves restart closes admission before watch
// arming and that a stale Ready cannot reopen a newer generation.
func TestBatchReadinessGeneration(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)
	txid := chainhash.Hash{0xf5}
	record := &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			0x00,
		},
		RegistrationStage:     batchcanon.RegistrationRegistering,
		ObservationGeneration: 1,
		State:                 batchcanon.StateProvisional,
		CSVExpiryDelta:        144,
		ConfirmationPkScript: []byte{
			0x51,
			0x20,
			0xf5,
		},
		ConsumedInputs: []batchcanon.ConsumedInput{
			consumedInput(outpoint(0x21, 0)),
		},
	}
	require.NoError(t, store.RegisterBatch(ctx, record, nil))
	require.NoError(t, store.MarkReady(ctx, txid, 1))

	ready, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, ready.Ready())

	reconciling, err := store.BeginReconcile(ctx, txid)
	require.NoError(t, err)
	require.Equal(t, uint64(2), reconciling.ObservationGeneration)
	require.Equal(
		t, batchcanon.RegistrationReconciling,
		reconciling.RegistrationStage,
	)
	require.False(t, reconciling.Ready())

	require.Error(t, store.MarkReady(ctx, txid, 1))
	stillClosed, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.False(t, stillClosed.Ready())

	require.NoError(t, store.MarkReady(ctx, txid, 2))
	ready, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.True(t, ready.Ready())
}

// TestBatchObservationIsAtomicAndGenerationGuarded proves an incomplete or
// stale snapshot cannot update even one input ahead of the batch state, while
// a complete current snapshot installs readiness with one revision change.
func TestBatchObservationIsAtomicAndGenerationGuarded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBatchCanonicalityStoreForTest(t)
	txid := chainhash.Hash{0xf6}
	inputA := consumedInput(outpoint(0x31, 0))
	inputB := consumedInput(outpoint(0x32, 1))
	record := &batchcanon.Record{
		BatchTxID: txid,
		BatchTx: []byte{
			0x00,
		},
		RegistrationStage:     batchcanon.RegistrationReconciling,
		ObservationGeneration: 2,
		State:                 batchcanon.StateUnseen,
		CSVExpiryDelta:        144,
		ConfirmationPkScript: []byte{
			0x51,
		},
		ConsumedInputs: []batchcanon.ConsumedInput{
			inputA, inputB,
		},
	}
	require.NoError(t, store.UpsertBatch(ctx, record))

	snapshot := &batchcanon.ObservationSnapshot{
		BatchTxID:          txid,
		Generation:         1,
		State:              batchcanon.StateConflictProvisional,
		ConfirmationHeight: fn.Some[int32](200),
		ConfirmationBlock:  fn.Some(chainhash.Hash{0x44}),
		Inputs: []batchcanon.InputObservation{
			{
				Outpoint:    inputA.Outpoint,
				Conflicting: true,
			},
			{
				Outpoint: inputB.Outpoint,
			},
		},
		Ready: true,
	}

	// Input rows are updated before the generation-guarded batch row inside
	// the SQL transaction. A stale generation must roll all of them back.
	require.Error(t, store.ApplyObservation(ctx, snapshot))
	unchanged, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(t, batchcanon.StateUnseen, unchanged.State)
	require.False(t, unchanged.Ready())
	require.False(t, inputFlags(t, unchanged, inputA.Outpoint).Conflicting)
	require.Equal(t, uint64(0), unchanged.Revision)

	// A caller cannot omit one immutable input from its supposedly complete
	// snapshot either.
	snapshot.Generation = 2
	snapshot.Inputs = snapshot.Inputs[:1]
	require.Error(t, store.ApplyObservation(ctx, snapshot))
	unchanged, err = store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.False(t, inputFlags(t, unchanged, inputA.Outpoint).Conflicting)

	snapshot.Inputs = append(snapshot.Inputs, batchcanon.InputObservation{
		Outpoint: inputB.Outpoint,
	})
	require.NoError(t, store.ApplyObservation(ctx, snapshot))
	installed, err := store.GetBatch(ctx, txid)
	require.NoError(t, err)
	require.Equal(
		t, batchcanon.StateConflictProvisional, installed.State,
	)
	require.Equal(t, int32(200), installed.ConfirmationHeight.UnwrapOr(0))
	require.True(t, inputFlags(t, installed, inputA.Outpoint).Conflicting)
	require.True(t, installed.Ready())
	require.Equal(t, uint64(1), installed.Revision)
}

// TestResolveConsumerEdgeRestoresExactOwner proves the happy-path restore is
// atomic, increments the business revision, clears the ownership marker, and
// is idempotent after the exact edge is complete.
func TestResolveConsumerEdgeRestoresExactOwner(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.registerConsumer(t, batchcanon.StateConflictFinalized)

	resolution, err := h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeRestored, resolution)

	restored, err := h.vtxos.GetVTXO(t.Context(), h.edge.ConsumedVTXO)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, restored.Status)
	require.Equal(t, h.edge.ExpectedRevision+1, restored.BusinessRevision)
	require.True(t, restored.ForfeitConsumerBatch.IsNone())

	pending, err := h.canon.ListPendingConsumerEdges(
		t.Context(), h.consumer,
	)
	require.NoError(t, err)
	require.Empty(t, pending)

	resolution, err = h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)
}

// TestResolveConsumerEdgeRequiresTerminalConsumer proves the storage-layer
// transaction cannot restore before the consumer is durably ready and
// conflict-finalized, even if a caller invokes it out of order.
func TestResolveConsumerEdgeRequiresTerminalConsumer(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.registerConsumer(t, batchcanon.StateProvisional)

	resolution, err := h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

	unchanged, err := h.vtxos.GetVTXO(t.Context(), h.edge.ConsumedVTXO)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeited, unchanged.Status)
	require.Equal(t, h.edge.ExpectedRevision, unchanged.BusinessRevision)

	require.NoError(
		t,
		h.canon.UpdateBatchState(
			t.Context(), h.consumer,
			batchcanon.StateConflictFinalized,
		),
	)
	resolution, err = h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeRestored, resolution)
}

// TestResolveConsumerEdgeRejectsStaleOwnership exercises the business-state
// predicates that keep a stale or competing edge from reviving value.
func TestResolveConsumerEdgeRejectsStaleOwnership(t *testing.T) {
	t.Parallel()

	t.Run("business revision", func(t *testing.T) {
		h := newConsumerRestoreHarness(t)
		h.edge.ExpectedRevision++
		h.registerConsumer(t, batchcanon.StateConflictFinalized)

		resolution, err := h.canon.ResolveConsumerEdge(
			t.Context(), h.edge, true,
		)
		require.NoError(t, err)
		require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

		unchanged, err := h.vtxos.GetVTXO(
			t.Context(), h.edge.ConsumedVTXO,
		)
		require.NoError(t, err)
		require.Equal(t, vtxo.VTXOStatusForfeited, unchanged.Status)
		require.Equal(
			t, h.edge.ExpectedRevision-1,
			unchanged.BusinessRevision,
		)
	})

	t.Run("consumer marker", func(t *testing.T) {
		h := newConsumerRestoreHarness(t)
		h.edge.ConsumerBatch = chainhash.Hash{0xc7}
		h.registerConsumer(t, batchcanon.StateConflictFinalized)

		resolution, err := h.canon.ResolveConsumerEdge(
			t.Context(), h.edge, true,
		)
		require.NoError(t, err)
		require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

		unchanged, err := h.vtxos.GetVTXO(
			t.Context(), h.edge.ConsumedVTXO,
		)
		require.NoError(t, err)
		require.Equal(t, vtxo.VTXOStatusForfeited, unchanged.Status)
		require.Equal(
			t, h.consumer,
			unchanged.ForfeitConsumerBatch.UnwrapOr(
				chainhash.Hash{},
			),
		)
	})

	t.Run("completed spend", func(t *testing.T) {
		h := newConsumerRestoreHarness(t)
		h.registerConsumer(t, batchcanon.StateConflictFinalized)
		require.NoError(
			t,
			h.vtxos.UpdateVTXOStatus(
				t.Context(), h.edge.ConsumedVTXO,
				vtxo.VTXOStatusSpent,
			),
		)

		resolution, err := h.canon.ResolveConsumerEdge(
			t.Context(), h.edge, true,
		)
		require.NoError(t, err)
		require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

		spent, err := h.vtxos.GetVTXO(
			t.Context(), h.edge.ConsumedVTXO,
		)
		require.NoError(t, err)
		require.Equal(t, vtxo.VTXOStatusSpent, spent.Status)
	})
}

// TestResolveConsumerEdgeBlocksReservation proves a durable reservation owns
// the candidate until it is objectively released.
func TestResolveConsumerEdgeBlocksReservation(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.registerConsumer(t, batchcanon.StateConflictFinalized)
	require.NoError(
		t,
		h.db.UpsertSpendingReservation(
			t.Context(), sqlc.UpsertSpendingReservationParams{
				OutpointHash:  h.edge.ConsumedVTXO.Hash[:],
				OutpointIndex: int32(h.edge.ConsumedVTXO.Index),
				OwnerKind:     1,
				OwnerID:       bytes.Repeat([]byte{0x77}, 32),
				CreatedAt:     1,
			},
		),
	)

	resolution, err := h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

	require.NoError(
		t,
		h.db.DeleteSpendingReservation(
			t.Context(), sqlc.DeleteSpendingReservationParams{
				OutpointHash:  h.edge.ConsumedVTXO.Hash[:],
				OutpointIndex: int32(h.edge.ConsumedVTXO.Index),
			},
		),
	)
	resolution, err = h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeRestored, resolution)
}

// TestResolveConsumerEdgeBlocksOtherViableConsumer proves a second
// provisional/final owner prevents restore, while an objectively invalidated
// competing edge no longer claims the value.
func TestResolveConsumerEdgeBlocksOtherViableConsumer(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.registerConsumer(t, batchcanon.StateConflictFinalized)

	other := h.edge
	other.ConsumerBatch = chainhash.Hash{0xc7}
	require.NoError(
		t,
		h.canon.RegisterBatch(
			t.Context(), readyBatchRecord(
				other.ConsumerBatch,
				batchcanon.StateProvisional,
			),
			[]batchcanon.ConsumerEdge{other},
		),
	)

	resolution, err := h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

	require.NoError(
		t,
		h.canon.UpdateBatchState(
			t.Context(), other.ConsumerBatch,
			batchcanon.StateConflictFinalized,
		),
	)
	resolution, err = h.canon.ResolveConsumerEdge(
		t.Context(), h.edge, true,
	)
	require.NoError(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeRestored, resolution)

	otherPending, err := h.canon.ListPendingConsumerEdges(
		t.Context(), other.ConsumerBatch,
	)
	require.NoError(t, err)
	require.Len(t, otherPending, 1)
}

// TestResolveConsumerEdgeRollsBackWithoutExactEdge proves the VTXO update and
// edge completion are one transaction. If exact edge deletion fails after a
// successful CAS, the VTXO marker remains forfeited.
func TestResolveConsumerEdgeRollsBackWithoutExactEdge(t *testing.T) {
	t.Parallel()

	h := newConsumerRestoreHarness(t)
	h.edge.ExpectedRevision++
	h.registerConsumer(t, batchcanon.StateConflictFinalized)

	attempt := h.edge
	attempt.ExpectedRevision--
	resolution, err := h.canon.ResolveConsumerEdge(
		t.Context(), attempt, true,
	)
	require.Error(t, err)
	require.Equal(t, batchcanon.ConsumerEdgeDeferred, resolution)

	unchanged, err := h.vtxos.GetVTXO(t.Context(), attempt.ConsumedVTXO)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeited, unchanged.Status)
	require.Equal(t, attempt.ExpectedRevision, unchanged.BusinessRevision)
	require.Equal(
		t, h.consumer,
		unchanged.ForfeitConsumerBatch.UnwrapOr(
			chainhash.Hash{},
		),
	)

	pending, err := h.canon.ListPendingConsumerEdges(
		t.Context(), h.consumer,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, h.edge.ExpectedRevision, pending[0].ExpectedRevision)
}

// TestConsumerEdgeEvidenceIsImmutable proves a repeat cannot change the exact
// revision or creator lineage used by terminal restoration.
func TestConsumerEdgeEvidenceIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*batchcanon.ConsumerEdge)
	}{
		{
			name: "business revision",
			mutate: func(edge *batchcanon.ConsumerEdge) {
				edge.ExpectedRevision++
			},
		},
		{
			name: "creator lineage",
			mutate: func(edge *batchcanon.ConsumerEdge) {
				edge.CreatorLineage = []chainhash.Hash{
					{
						0xd7,
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newConsumerRestoreHarness(t)
			h.registerConsumer(t, batchcanon.StateConflictFinalized)

			changed := h.edge
			test.mutate(&changed)
			err := h.canon.RegisterBatch(
				t.Context(), readyBatchRecord(
					h.consumer,
					batchcanon.StateConflictFinalized,
				),
				[]batchcanon.ConsumerEdge{changed},
			)
			require.ErrorIs(
				t, err, batchcanon.ErrRegistrationConflict,
			)

			record, err := h.canon.GetBatch(t.Context(), h.consumer)
			require.NoError(t, err)
			require.Equal(
				t, batchcanon.RegistrationQuarantined,
				record.RegistrationStage,
			)
			require.False(t, record.Ready())

			pending, err := h.canon.ListPendingConsumerEdges(
				t.Context(), h.consumer,
			)
			require.NoError(t, err)
			require.Equal(
				t, []batchcanon.ConsumerEdge{h.edge}, pending,
			)
		})
	}
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
	require.False(t, rec0.Ready())
	require.False(t, rec0.EvidenceComplete())
	require.Equal(
		t, batchcanon.RegistrationReconciling, rec0.RegistrationStage,
	)
	require.Equal(t, int32(500), rec0.ConfirmationHeight.UnwrapOr(0))
	require.Equal(t, int32(500), rec0.CSVExpiryDelta)
	require.Equal(t, int32(1000), rec0.EffectiveExpiry().UnwrapOr(0))
	require.Equal(t, []wire.OutPoint{desc0.Outpoint}, rec0.DependentVTXOs)

	rec1, err := canon.GetBatch(ctx, desc1.CommitmentTxID)
	require.NoError(t, err)
	require.Equal(t, batchcanon.StateProvisional, rec1.State)
	require.False(t, rec1.Ready())
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

	// The first authenticated producer registration atomically completes an
	// upgrade placeholder. Age-derived state is discarded, a fresh
	// generation starts fail-closed, and dependents learned from the old DB
	// are retained alongside newly registered ones.
	input := wire.OutPoint{Hash: chainhash.Hash{0xe1}, Index: 2}
	watchScript := []byte{0x51, 0x20, 0xe2}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&input, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1_000, watchScript))
	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))
	newDependent := wire.OutPoint{
		Hash: chainhash.Hash{
			0xe3,
		},
		Index: 1,
	}
	require.NoError(
		t,
		canon.RegisterBatch(
			ctx, &batchcanon.Record{
				BatchTxID:            desc0.CommitmentTxID,
				BatchTx:              raw.Bytes(),
				BatchOutputIndex:     0,
				ConfirmationPkScript: watchScript,
				CSVExpiryDelta:       500,
				ConsumedInputs: []batchcanon.ConsumedInput{
					{
						Outpoint: input,
						Value:    1_100,
						PkScript: []byte{0x51},
					},
				},
				DependentVTXOs: []wire.OutPoint{
					newDependent,
				},
			},
			nil,
		),
	)

	rec0, err = canon.GetBatch(ctx, desc0.CommitmentTxID)
	require.NoError(t, err)
	require.True(t, rec0.EvidenceComplete())
	require.False(t, rec0.Ready())
	require.Equal(t, batchcanon.StateUnseen, rec0.State)
	require.Equal(
		t, batchcanon.RegistrationRegistering, rec0.RegistrationStage,
	)
	require.Equal(t, uint64(2), rec0.ObservationGeneration)
	require.True(t, rec0.ConfirmationHeight.IsNone())
	require.ElementsMatch(
		t, []wire.OutPoint{desc0.Outpoint, newDependent},
		rec0.DependentVTXOs,
	)
}
