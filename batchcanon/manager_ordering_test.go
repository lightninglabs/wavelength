package batchcanon

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestDeriveStatePriority pins the reducer's complete pre-terminal priority
// table. The manager may receive observations for distinct subjects in any
// order, but the complete snapshot must always reduce to the same dominant
// semantic fact.
func TestDeriveStatePriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		conf   confState
		inputs []*inputWatch
		want   State
	}{
		{
			name: "unseen",
			conf: confUnseen,
			want: StateUnseen,
		},
		{
			name: "confirmed",
			conf: confConfirmed,
			want: StateProvisional,
		},
		{
			name: "finalized",
			conf: confFinalized,
			want: StateFinalized,
		},
		{
			name: "reorged out",
			conf: confReorgedOut,
			want: StateReorgedOut,
		},
		{
			name: "conflict dominates confirmation",
			conf: confConfirmed,
			inputs: []*inputWatch{
				{
					conflicting: true,
				},
			},
			want: StateConflictProvisional,
		},
		{
			name: "conflict dominates reorg",
			conf: confReorgedOut,
			inputs: []*inputWatch{
				{
					conflicting: true,
				},
			},
			want: StateConflictProvisional,
		},
		{
			name: "final conflict dominates every provisional fact",
			conf: confReorgedOut,
			inputs: []*inputWatch{
				{
					conflicting: true,
				},
				{
					conflictFinal: true,
				},
			},
			want: StateConflictFinalized,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			watch := &batchWatch{
				conf:   test.conf,
				inputs: make(map[wire.OutPoint]*inputWatch),
			}
			for i, input := range test.inputs {
				watch.inputs[testOutpoint(byte(i+1), 0)] = input
			}

			require.Equal(t, test.want, deriveState(watch))
		})
	}
}

// TestSpendDoneBeforeSpendRetainsTerminalEvidence proves the manager remains
// fail-closed when independently delivered finality and identity callbacks
// reach its mailbox out of order. Done alone cannot classify the spender, but
// the later Spend must still finalize a real conflict rather than strand it in
// the provisional state forever.
func TestSpendDoneBeforeSpendRetainsTerminalEvidence(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x81)
	input := testOutpoint(0x82, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x81},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x83))

	h.fireSpendDone(t, input)
	got := h.state(t, txid).Record
	require.Equal(t, StateProvisional, got.State)
	require.False(t, got.Ready())

	h.fireSpend(t, input, testBatchTxid(0x84), 102)
	got = h.state(t, txid).Record
	require.Equal(t, StateConflictFinalized, got.State)
	require.True(t, got.Ready())
}

// TestSpendDoneBeforeOwnSpendDoesNotInventConflict proves identity still
// controls classification when finality is delivered first.
func TestSpendDoneBeforeOwnSpendDoesNotInventConflict(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x85)
	input := testOutpoint(0x86, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x85},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x87))

	h.fireSpendDone(t, input)
	h.fireSpend(t, input, txid, 101)
	got := h.state(t, txid).Record
	require.Equal(t, StateProvisional, got.State)
	require.True(t, got.Ready())
}

// TestSpendReorgClearsEarlyDone proves objective cancellation evidence clears
// a pending finality signal before a later replacement spend is classified.
func TestSpendReorgClearsEarlyDone(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x88)
	input := testOutpoint(0x89, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x88},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x8a))

	h.fireSpendDone(t, input)
	h.fireSpendReorged(t, input)
	h.fireSpend(t, input, testBatchTxid(0x8b), 102)
	got := h.state(t, txid).Record
	require.Equal(t, StateConflictProvisional, got.State)
	require.True(t, got.Ready())
}

// TestConflictFinalityIsStickyAcrossLateEvents proves terminal invalidation
// cannot be undone by a late reorg callback, while still allowing untouched
// subjects to complete the current Ready generation.
func TestConflictFinalityIsStickyAcrossLateEvents(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0x91)
	conflicted := testOutpoint(0x92, 0)
	late := testOutpoint(0x93, 0)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0x91},
		ConsumedInputs: []ConsumedInput{
			ci(conflicted), ci(late),
		},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0x94))
	h.fireSpend(t, conflicted, testBatchTxid(0x95), 102)
	h.fireSpendDone(t, conflicted)

	got := h.state(t, txid).Record
	require.Equal(t, StateConflictFinalized, got.State)
	require.False(t, got.Ready(), "the second input is still unobserved")

	// This ordering is impossible as a coherent best-chain history after
	// policy finality, but queued callbacks must still be harmless. The
	// late reorg can satisfy subject observation; it cannot erase final
	// evidence.
	h.fireSpendReorged(t, conflicted)
	require.Equal(
		t, StateConflictFinalized, h.state(t, txid).Record.State,
	)

	// The remaining subject may arrive after terminal evidence. It
	// completes Ready(g) without changing the sticky invalidation.
	h.fireSpend(t, late, txid, 102)
	got = h.state(t, txid).Record
	require.Equal(t, StateConflictFinalized, got.State)
	require.True(t, got.Ready())

	// Even callbacks already queued before synchronous watch cleanup cannot
	// turn the batch usable again.
	h.fireConfReorged(t, txid)
	h.fireConfirmed(t, txid, 103, testBatchTxid(0x96))
	got = h.state(t, txid).Record
	require.Equal(t, StateConflictFinalized, got.State)
	require.True(t, got.Ready())
}

// TestBatchFinalityDominatesContradictoryLateInput proves the symmetric
// terminal ordering: once the batch confirmation is policy-final, a queued
// contradictory input notification cannot flip it to invalidated or limbo.
func TestBatchFinalityDominatesContradictoryLateInput(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xa1)
	inputs := []wire.OutPoint{
		testOutpoint(0xa2, 0),
		testOutpoint(0xa3, 0),
	}
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xa1},
		ConsumedInputs: []ConsumedInput{
			ci(inputs[0]), ci(inputs[1]),
		},
	})
	h.fireConfirmed(t, txid, 101, testBatchTxid(0xa4))
	h.fireConfDone(t, txid)
	require.Equal(t, StateFinalized, h.state(t, txid).Record.State)

	// The first late callback claims a different spender. Policy-final
	// batch evidence is sticky, so the callback only contributes to
	// readiness.
	h.fireSpend(t, inputs[0], testBatchTxid(0xff), 102)
	h.fireSpend(t, inputs[1], txid, 101)
	got := h.state(t, txid).Record
	require.Equal(t, StateFinalized, got.State)
	require.True(t, got.Ready())
}

// TestBatchDoneBeforeConfirmedRetainsLateBlockIdentity proves independently
// delivered finality cannot strand a finalized record without the confirmation
// metadata needed for expiry derivation. The late callback may enrich the
// terminal record, but must never reopen or reclassify it.
func TestBatchDoneBeforeConfirmedRetainsLateBlockIdentity(t *testing.T) {
	t.Parallel()

	h := newManagerHarness(t, 100)
	txid := testBatchTxid(0xb1)
	input := testOutpoint(0xb2, 0)
	block := testBatchTxid(0xb3)
	h.registerBatch(t, &RegisterBatchRequest{
		BatchTxID:            txid,
		ConfirmationPkScript: []byte{0x51, 0x20, 0xb1},
		ConsumedInputs:       []ConsumedInput{ci(input)},
	})

	h.fireConfDone(t, txid)
	h.fireSpend(t, input, txid, 101)
	got := h.state(t, txid).Record
	require.Equal(t, StateFinalized, got.State)
	require.True(t, got.Ready())
	require.True(t, got.ConfirmationHeight.IsNone())
	require.True(t, got.ConfirmationBlock.IsNone())

	h.fireConfirmed(t, txid, 101, block)
	got = h.state(t, txid).Record
	require.Equal(t, StateFinalized, got.State)
	require.True(t, got.Ready())
	require.Equal(t, int32(101), got.ConfirmationHeight.UnwrapOr(0))
	require.True(t, got.ConfirmationBlock.IsSome())
	require.Equal(
		t, block,
		got.ConfirmationBlock.UnwrapOr(
			testBatchTxid(0),
		),
	)
}
