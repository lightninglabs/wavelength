package batchcanon

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestReconcileLeavesUpgradePlaceholderFailClosed proves an incomplete
// historical row never aborts daemon startup and is never treated as armed or
// ready. It waits for authenticated producer evidence to complete the row.
func TestReconcileLeavesUpgradePlaceholderFailClosed(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := chainhash.Hash{0xd1}
	require.NoError(
		t,
		h.store.UpsertBatch(
			t.Context(), &Record{
				BatchTxID:             txid,
				RegistrationStage:     RegistrationReconciling,
				ObservationGeneration: 1,
				ReadyGeneration:       fn.None[uint64](),
				State:                 StateProvisional,
			},
		),
	)

	require.NoError(t, h.mgr.Reconcile(t.Context()))
	record, err := h.store.GetBatch(t.Context(), txid)
	require.NoError(t, err)
	require.False(t, record.EvidenceComplete())
	require.False(t, record.Ready())
	require.Equal(t, uint64(1), record.ObservationGeneration)
	require.NotContains(t, h.mgr.watches, txid)
}

// TestRegistrationCompletesUpgradePlaceholder proves authenticated evidence
// replaces an age-derived historical placeholder in a fresh generation and
// arms every real subject instead of trusting the placeholder's old state.
func TestRegistrationCompletesUpgradePlaceholder(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	req := validRegistrationRequest(t)
	historicalDependent := testOutpoint(0xd2, 0)
	require.NoError(
		t,
		h.store.UpsertBatch(
			t.Context(), &Record{
				BatchTxID:             req.BatchTxID,
				RegistrationStage:     RegistrationReconciling,
				ObservationGeneration: 1,
				ReadyGeneration:       fn.None[uint64](),
				State:                 StateFinalized,
				DependentVTXOs: []wire.OutPoint{
					historicalDependent,
				},
			},
		),
	)
	req.DependentVTXOs = []wire.OutPoint{testOutpoint(0xd3, 0)}

	h.registerBatch(t, req)
	record, err := h.store.GetBatch(t.Context(), req.BatchTxID)
	require.NoError(t, err)
	require.True(t, record.EvidenceComplete())
	require.False(t, record.Ready())
	require.Equal(t, StateUnseen, record.State)
	require.Equal(t, RegistrationRegistering, record.RegistrationStage)
	require.Equal(t, uint64(2), record.ObservationGeneration)
	require.ElementsMatch(
		t,
		[]wire.OutPoint{
			historicalDependent, req.DependentVTXOs[0],
		},
		record.DependentVTXOs,
	)

	h.mock.getConfRefs(t, req.BatchTxID)
	for _, input := range req.ConsumedInputs {
		h.mock.getSpendRefs(t, input.Outpoint)
	}
}

// TestManagerIgnoresCallbacksFromPriorGeneration proves queued events from a
// released watch set cannot contaminate the fresh restart snapshot or satisfy
// Ready(g) after reconciliation advances the durable generation.
func TestManagerIgnoresCallbacksFromPriorGeneration(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xd7)
	input := testOutpoint(0xd8, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xd7},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0xe1))
	h.fireSpend(t, input, txid, 101)
	require.True(t, h.state(t, txid).Record.Ready())

	oldConf := h.mock.getConfRefs(t, txid)
	oldSpend := h.mock.getSpendRefs(t, input)[0]

	// Model process restart: the old actor callbacks can still be queued,
	// while the new manager generation closes admission before re-arming.
	delete(h.mgr.watches, txid)
	reconciling, err := h.store.BeginReconcile(t.Context(), txid)
	require.NoError(t, err)
	require.Equal(t, uint64(2), reconciling.ObservationGeneration)
	require.NoError(t, h.mgr.reconcileOne(t.Context(), reconciling))

	require.NoError(
		t,
		oldConf.reorged.Tell(
			t.Context(), chainsource.ConfReorgedEvent{
				Txid: txid,
			},
		),
	)
	require.NoError(
		t,
		oldSpend.reorged.Tell(
			t.Context(), chainsource.SpendReorgedEvent{
				Outpoint: input,
			},
		),
	)

	// Drain the stale callbacks. The durable observation remains untouched
	// and readiness stays closed for generation 2.
	got := h.state(t, txid).Record
	require.Equal(t, StateProvisional, got.State)
	require.Equal(t, int32(101), got.ConfirmationHeight.UnwrapOr(0))
	require.False(t, got.Ready())

	// Only callbacks from the freshly armed watch set can complete
	// Ready(2).
	h.fireConfirmed(t, txid, 102, testBatchTxid(0xe2))
	h.fireSpend(t, input, txid, 102)
	got = h.state(t, txid).Record
	require.True(t, got.Ready())
	require.Equal(t, uint64(2), got.ObservationGeneration)
	require.Equal(t, int32(102), got.ConfirmationHeight.UnwrapOr(0))
}
