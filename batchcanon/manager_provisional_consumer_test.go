package batchcanon

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// restoreRecorder captures VTXO actors activated after an atomic store restore.
type restoreRecorder struct {
	mu       sync.Mutex
	restored []wire.OutPoint
}

func (r *restoreRecorder) restore(_ context.Context, op wire.OutPoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored = append(r.restored, op)

	return nil
}

func (r *restoreRecorder) outpoints() []wire.OutPoint {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]wire.OutPoint(nil), r.restored...)
}

// TestManagerRestoresForfeitedVTXOOnConflictFinalized proves the
// reverse-dependency (provisional-forfeit) restore: when a batch that
// provisionally forfeits a VTXO is invalidated by a finalized conflict, the
// manager restores that VTXO via the RestoreConsumedVTXO callback and drops the
// edge. A transient conflict (not yet finalized) must NOT restore -- the
// forfeit is only reversed once the invalidation is final.
func TestManagerRestoresForfeitedVTXOOnConflictFinalized(t *testing.T) {
	t.Parallel()

	rec := &restoreRecorder{}
	h := newManagerHarnessWithRestore(t, 100, rec.restore)

	// A round-2 commitment batch that forfeits a round-1 VTXO and spends a
	// consumed input we can double-spend.
	consumerBatch := testBatchTxid(0xc2)
	creatorBatch := testBatchTxid(0xc1)
	forfeitedVTXO := testOutpoint(0xa1, 0)
	consumedInput := testOutpoint(0x1e, 1)
	creatorInput := testOutpoint(0x1d, 1)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            creatorBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xc1},
		ConsumedInputs:       []ConsumedInput{ci(creatorInput)},
	})
	h.fireConfirmed(t, creatorBatch, 100, testBatchTxid(0xb0))
	h.fireSpend(t, creatorInput, creatorBatch, 100)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            consumerBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xc2},
		ConsumedInputs:       []ConsumedInput{ci(consumedInput)},
		ConsumedVTXOs: []ConsumerEdge{
			{
				ConsumedVTXO:     forfeitedVTXO,
				ExpectedRevision: 2,
				CreatorLineage: []chainhash.Hash{
					creatorBatch,
				},
			},
		},
	})

	// Confirm the batch, then observe a conflicting spend of its input. A
	// non-final conflict must not restore yet.
	h.fireConfirmed(t, consumerBatch, 101, testBatchTxid(0xb1))
	h.fireSpend(t, consumedInput, testBatchTxid(0x9e), 102)
	require.Equal(
		t, StateConflictProvisional,
		h.state(t, consumerBatch).Record.State,
	)
	require.Empty(
		t, rec.outpoints(),
		"a provisional (non-final) conflict must not restore the "+
			"forfeited VTXO",
	)

	// Mature the conflicting spend past the reorg-safety depth: the batch
	// is now permanently invalidated, so its forfeit is reversed.
	h.fireSpendDone(t, consumedInput)
	require.Equal(
		t, StateConflictFinalized,
		h.state(t, consumerBatch).Record.State,
	)
	require.Equal(
		t, []wire.OutPoint{forfeitedVTXO}, rec.outpoints(),
		"a finalized conflict must restore the forfeited VTXO",
	)

	// The edge is dropped after restoring, so the restore fires at most
	// once even if the state is re-derived.
	remaining, err := h.store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(t, remaining, "edges must be cleared after restore")
}

// TestManagerClearsForfeitEdgesOnFinalized proves the other half of the
// lifecycle: when the consumer batch becomes canonical and final, the forfeit
// is permanent, so the reverse-dependency edges are dropped WITHOUT restoring.
func TestManagerClearsForfeitEdgesOnFinalized(t *testing.T) {
	t.Parallel()

	rec := &restoreRecorder{}
	h := newManagerHarnessWithRestore(t, 100, rec.restore)

	consumerBatch := testBatchTxid(0xf2)
	creatorBatch := testBatchTxid(0xf1)
	forfeitedVTXO := testOutpoint(0xa2, 0)
	input := testOutpoint(0xa3, 0)
	creatorInput := testOutpoint(0xa4, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            creatorBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xf1},
		ConsumedInputs:       []ConsumedInput{ci(creatorInput)},
	})
	h.fireConfirmed(t, creatorBatch, 100, testBatchTxid(0xb1))
	h.fireSpend(t, creatorInput, creatorBatch, 100)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            consumerBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xf2},
		ConsumedInputs:       []ConsumedInput{ci(input)},
		ConsumedVTXOs: []ConsumerEdge{
			{
				ConsumedVTXO:     forfeitedVTXO,
				ExpectedRevision: 2,
				CreatorLineage: []chainhash.Hash{
					creatorBatch,
				},
			},
		},
	})

	// Confirm then finalize the batch on the canonical chain.
	h.fireConfirmed(t, consumerBatch, 101, testBatchTxid(0xb2))
	h.fireConfDone(t, consumerBatch)
	h.fireSpend(t, input, consumerBatch, 101)
	require.Equal(
		t, StateFinalized, h.state(t, consumerBatch).Record.State,
	)

	require.Empty(
		t, rec.outpoints(),
		"a canonically finalized batch must not restore its "+
			"forfeited VTXO",
	)
	remaining, err := h.store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(
		t, remaining,
		"edges must be cleared once the forfeit is final and safe",
	)
}

// TestRepeatRegistrationDrivesTerminalConsumerEdge proves replay cannot add
// durable restore work behind a terminal batch after its last chain callback
// and leave that work stranded until restart.
func TestRepeatRegistrationDrivesTerminalConsumerEdge(t *testing.T) {
	t.Parallel()

	rec := &restoreRecorder{}
	h := newManagerHarnessWithRestore(t, 100, rec.restore)
	creatorBatch := testBatchTxid(0xb1)
	consumerBatch := testBatchTxid(0xb2)
	creatorInput := testOutpoint(0xb3, 0)
	consumerInput := testOutpoint(0xb4, 0)
	forfeitedVTXO := testOutpoint(0xb5, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            creatorBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xb1},
		ConsumedInputs:       []ConsumedInput{ci(creatorInput)},
	})
	h.fireConfirmed(t, creatorBatch, 100, testBatchTxid(0xb6))
	h.fireSpend(t, creatorInput, creatorBatch, 100)

	consumerReq := &RegisterBatchRequest{
		BatchTxID: consumerBatch,
		ConfirmationPkScript: []byte{
			0x51,
			0x20,
			0xb2,
		},
		ConsumedInputs: []ConsumedInput{
			ci(consumerInput),
		},
	}
	h.registerBatch(t, consumerReq)
	h.fireConfirmed(t, consumerBatch, 101, testBatchTxid(0xb7))
	h.fireSpend(t, consumerInput, testBatchTxid(0xb8), 102)
	h.fireSpendDone(t, consumerInput)
	require.Equal(
		t, StateConflictFinalized,
		h.state(t, consumerBatch).Record.State,
	)

	consumerReq.ConsumedVTXOs = []ConsumerEdge{
		{
			ConsumedVTXO:     forfeitedVTXO,
			ExpectedRevision: 2,
			CreatorLineage: []chainhash.Hash{
				creatorBatch,
			},
		},
	}
	h.registerBatch(t, consumerReq)

	require.Equal(t, []wire.OutPoint{forfeitedVTXO}, rec.outpoints())
	remaining, err := h.store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(t, remaining)
}

// TestCreatorRecoveryRedrivesTerminalConsumerEdge proves a consumed VTXO is
// restored as soon as its own creator lineage becomes objectively usable. A
// daemon restart or replay of the user's old operation is not required.
func TestCreatorRecoveryRedrivesTerminalConsumerEdge(t *testing.T) {
	t.Parallel()

	rec := &restoreRecorder{}
	h := newManagerHarnessWithRestore(t, 100, rec.restore)
	creatorBatch := testBatchTxid(0xc3)
	consumerBatch := testBatchTxid(0xc4)
	creatorInput := testOutpoint(0xc5, 0)
	consumerInput := testOutpoint(0xc6, 0)
	forfeitedVTXO := testOutpoint(0xc7, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            creatorBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xc3},
		ConsumedInputs:       []ConsumedInput{ci(creatorInput)},
	})
	h.fireConfirmed(t, creatorBatch, 100, testBatchTxid(0xc8))
	h.fireSpend(t, creatorInput, creatorBatch, 100)
	h.fireConfReorged(t, creatorBatch)
	require.Equal(t, StateReorgedOut, h.state(t, creatorBatch).Record.State)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            consumerBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xc4},
		ConsumedInputs:       []ConsumedInput{ci(consumerInput)},
		ConsumedVTXOs: []ConsumerEdge{
			{
				ConsumedVTXO:     forfeitedVTXO,
				ExpectedRevision: 2,
				CreatorLineage: []chainhash.Hash{
					creatorBatch,
				},
			},
		},
	})
	h.fireConfirmed(t, consumerBatch, 101, testBatchTxid(0xc9))
	h.fireSpend(t, consumerInput, testBatchTxid(0xca), 102)
	h.fireSpendDone(t, consumerInput)
	require.Empty(t, rec.outpoints())

	remaining, err := h.store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Len(t, remaining, 1)

	// The creator reconfirms. Its ready observation is the evidence change
	// that unblocks the already-terminal consumer's restore checkpoint.
	h.fireConfirmed(t, creatorBatch, 103, testBatchTxid(0xcb))
	require.Equal(
		t, StateProvisional, h.state(t, creatorBatch).Record.State,
	)
	require.Equal(t, []wire.OutPoint{forfeitedVTXO}, rec.outpoints())
	remaining, err = h.store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(t, remaining)
}

// TestManagerReconcileRestoresInterruptedForfeit proves the crash-recovery
// half of the restore lifecycle: if a batch's restore failed partway through
// (RestoreConsumedVTXO errored, so the edges were deliberately kept for a
// retry) and the daemon then restarted, the retained edges must still be
// re-driven. Because conflict_finalized is terminal, its watches are not
// re-armed on Reconcile and the live spend-done event that first triggered the
// restore will never fire again -- so Reconcile itself must sweep terminal
// conflicts and complete any interrupted restore, otherwise the consumed VTXOs
// would stay forfeited forever (a permanent lock).
func TestManagerReconcileRestoresInterruptedForfeit(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	consumerBatch := testBatchTxid(0xd1)
	creatorBatch := testBatchTxid(0xd0)
	forfeitedVTXO := testOutpoint(0xa3, 0)

	// Seed the state a partial-failure restore leaves behind: the batch is
	// already conflict_finalized (persisted), yet its reverse-dependency
	// edge is still present because RestoreConsumedVTXO errored before the
	// edge could be dropped.
	creatorRecord := &Record{
		BatchTxID:             creatorBatch,
		RegistrationStage:     RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some[uint64](1),
		State:                 StateFinalized,
	}
	completeTestRecordEvidence(creatorRecord)
	require.NoError(
		t, store.UpsertBatch(t.Context(), creatorRecord),
	)
	consumerRecord := &Record{
		BatchTxID:             consumerBatch,
		RegistrationStage:     RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some[uint64](1),
		State:                 StateConflictFinalized,
		ConfirmationHeight:    fn.Some[int32](101),
		CSVExpiryDelta:        50,
	}
	completeTestRecordEvidence(consumerRecord)
	require.NoError(
		t,
		store.UpsertBatch(
			t.Context(), consumerRecord,
		),
	)
	require.NoError(
		t,
		store.RegisterBatch(
			t.Context(), consumerRecord, []ConsumerEdge{
				{
					ConsumedVTXO:     forfeitedVTXO,
					ExpectedRevision: 2,
					CreatorLineage: []chainhash.Hash{
						creatorBatch,
					},
				},
			},
		),
	)

	rec := &restoreRecorder{}

	mock := newMockChainSource(200)
	mockActor := actor.NewActor(actor.ActorConfig[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{ID: "mock", Behavior: mock, MailboxSize: 64})
	mockActor.Start()
	t.Cleanup(mockActor.Stop)

	mgr := NewManager(
		ManagerConfig{
			Store:                store,
			ChainSource:          mockActor.Ref(),
			ActivateRestoredVTXO: rec.restore,
		},
	)
	mgrActor := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "mgr", Behavior: mgr, MailboxSize: 64,
	})
	mgr.SetSelfRef(mgrActor.TellRef())
	mgrActor.Start()
	t.Cleanup(mgrActor.Stop)

	require.NoError(t, mgr.Reconcile(t.Context()))

	// The interrupted restore is completed and the edge dropped.
	require.Equal(
		t, []wire.OutPoint{forfeitedVTXO}, rec.outpoints(),
		"Reconcile must re-drive the interrupted restore for a "+
			"terminal conflict",
	)
	remaining, err := store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(t, remaining, "edges must be cleared after restore")
}

// TestManagerCompletesEdgeWithoutRestoringInvalidCreator proves a terminal
// consumer cannot revive a VTXO whose own creator lineage is invalidated. The
// edge is objectively finished without activating a Live VTXO actor.
func TestManagerCompletesEdgeWithoutRestoringInvalidCreator(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	consumerBatch := testBatchTxid(0xe1)
	creatorBatch := testBatchTxid(0xe0)
	consumedVTXO := testOutpoint(0xae, 0)

	creatorRecord := &Record{
		BatchTxID:             creatorBatch,
		RegistrationStage:     RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some[uint64](1),
		State:                 StateConflictFinalized,
	}
	completeTestRecordEvidence(creatorRecord)
	require.NoError(
		t,
		store.RegisterBatch(
			t.Context(), creatorRecord, nil,
		),
	)

	consumerRecord := &Record{
		BatchTxID:             consumerBatch,
		RegistrationStage:     RegistrationComplete,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.Some[uint64](1),
		State:                 StateConflictFinalized,
	}
	completeTestRecordEvidence(consumerRecord)
	require.NoError(
		t,
		store.RegisterBatch(
			t.Context(), consumerRecord, []ConsumerEdge{
				{
					ConsumedVTXO:     consumedVTXO,
					ExpectedRevision: 2,
					CreatorLineage: []chainhash.Hash{
						creatorBatch,
					},
				},
			},
		),
	)

	rec := &restoreRecorder{}
	mock := newMockChainSource(200)
	mockActor := actor.NewActor(actor.ActorConfig[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]{ID: "mock", Behavior: mock, MailboxSize: 64})
	mockActor.Start()
	t.Cleanup(mockActor.Stop)

	mgr := NewManager(ManagerConfig{
		Store:                store,
		ChainSource:          mockActor.Ref(),
		ActivateRestoredVTXO: rec.restore,
	})
	mgrActor := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "mgr", Behavior: mgr, MailboxSize: 64,
	})
	mgr.SetSelfRef(mgrActor.TellRef())
	mgrActor.Start()
	t.Cleanup(mgrActor.Stop)

	require.NoError(t, mgr.Reconcile(t.Context()))
	require.Empty(
		t, rec.outpoints(),
		"invalid creator lineage must never activate a restored VTXO",
	)
	remaining, err := store.ListPendingConsumerEdges(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(
		t, remaining,
		"objectively invalid creator lineage completes the edge",
	)
}
