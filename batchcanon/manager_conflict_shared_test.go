package batchcanon

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestManagerSharedInputSpendClassifiesPerBatch verifies that when two batches
// consume the SAME input (the double-spend case), a spend by one batch's own tx
// is classified as the expected consumption for that batch and as a conflict
// for the OTHER batch — every watch on the outpoint is updated, not just one
// arbitrary batch. It also checks the finalize promotion is per-batch.
func TestManagerSharedInputSpendClassifiesPerBatch(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)

	txA := testBatchTxid(0xa1)
	txB := testBatchTxid(0xb2)
	shared := testOutpoint(0xcc, 0)

	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txA,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x01},
		ConsumedInputs:       []wire.OutPoint{shared},
	})
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txB,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x02},
		ConsumedInputs:       []wire.OutPoint{shared},
	})

	// Batch A wins the input and confirms; B never confirms.
	h.fireConfirmed(t, txA, 101, testBatchTxid(0x01))
	require.Equal(t, StateProvisional, h.state(t, txA).Record.State)

	// The shared input is spent by A's own tx: not a conflict for A, but a
	// conflicting double-spend for B (which wanted the same input).
	h.fireSpend(t, shared, txA, 101)

	require.Equal(
		t, StateProvisional, h.state(t, txA).Record.State,
		"batch whose own tx spent the input must not be in conflict",
	)
	require.Equal(
		t, StateConflictProvisional, h.state(t, txB).Record.State,
		"batch losing its input to another tx must be "+
			"conflict-provisional",
	)

	// Once the spend matures, only the conflicted batch (B) is promoted to
	// conflict-finalized; A's own consumption stays provisional.
	h.fireSpendDone(t, shared)

	require.Equal(
		t, StateProvisional, h.state(t, txA).Record.State,
		"self-consuming batch must not finalize as a conflict",
	)
	require.Equal(
		t, StateConflictFinalized, h.state(t, txB).Record.State,
		"conflicted batch must promote to conflict-finalized",
	)
}
