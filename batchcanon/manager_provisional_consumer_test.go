package batchcanon

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// restoreRecorder captures the VTXO outpoints the manager asks to restore via
// the RestoreConsumedVTXO callback.
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
	forfeitedVTXO := testOutpoint(0xa1, 0)
	consumedInput := testOutpoint(0x1e, 1)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            consumerBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xc2},
		ConsumedInputs:       []ConsumedInput{ci(consumedInput)},
		ForfeitedVTXOs:       []wire.OutPoint{forfeitedVTXO},
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
	remaining, err := h.store.ListProvisionalConsumersForBatch(
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
	forfeitedVTXO := testOutpoint(0xa2, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            consumerBatch,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xf2},
		ForfeitedVTXOs:       []wire.OutPoint{forfeitedVTXO},
	})

	// Confirm then finalize the batch on the canonical chain.
	h.fireConfirmed(t, consumerBatch, 101, testBatchTxid(0xb2))
	h.fireConfDone(t, consumerBatch)
	require.Equal(
		t, StateFinalized, h.state(t, consumerBatch).Record.State,
	)

	require.Empty(
		t, rec.outpoints(),
		"a canonically finalized batch must not restore its "+
			"forfeited VTXO",
	)
	remaining, err := h.store.ListProvisionalConsumersForBatch(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(
		t, remaining,
		"edges must be cleared once the forfeit is final and safe",
	)
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
	forfeitedVTXO := testOutpoint(0xa3, 0)

	// Seed the state a partial-failure restore leaves behind: the batch is
	// already conflict_finalized (persisted), yet its reverse-dependency
	// edge is still present because RestoreConsumedVTXO errored before the
	// edge could be dropped.
	require.NoError(
		t,
		store.UpsertBatch(
			t.Context(), &Record{
				BatchTxID:          consumerBatch,
				State:              StateConflictFinalized,
				ConfirmationHeight: fn.Some[int32](101),
				CSVExpiryDelta:     50,
			},
		),
	)
	require.NoError(
		t,
		store.AddProvisionalConsumer(
			t.Context(), forfeitedVTXO, consumerBatch,
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
			Store:               store,
			ChainSource:         mockActor.Ref(),
			RestoreConsumedVTXO: rec.restore,
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
	remaining, err := store.ListProvisionalConsumersForBatch(
		t.Context(), consumerBatch,
	)
	require.NoError(t, err)
	require.Empty(t, remaining, "edges must be cleared after restore")
}
