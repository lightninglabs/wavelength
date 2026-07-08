package batchcanon

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestAvailabilityForState pins the State -> Availability mapping.
func TestAvailabilityForState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state State
		want  Availability
	}{
		{
			StateFinalized,
			AvailableFinal,
		},
		{
			StateProvisional,
			AvailableProvisional,
		},
		{
			StateUnseen,
			AvailabilityUnknown,
		},
		{
			StateReorgedOut,
			LimboReorg,
		},
		{
			StateConflictProvisional,
			LimboConflict,
		},
		{
			StateConflictFinalized,
			Invalidated,
		},
	}

	for _, tc := range cases {
		require.Equal(
			t, tc.want, AvailabilityForState(tc.state),
			tc.state.String(),
		)
	}
}

// TestAvailabilityUsable verifies only confirmed lineage is usable.
func TestAvailabilityUsable(t *testing.T) {
	t.Parallel()

	require.True(t, AvailableFinal.Usable())
	require.True(t, AvailableProvisional.Usable())
	require.False(t, AvailabilityUnknown.Usable())
	require.False(t, LimboReorg.Usable())
	require.False(t, LimboConflict.Usable())
	require.False(t, Invalidated.Usable())
}

// TestCombineAvailability verifies a multi-parent lineage takes the worst
// (least-available) parent.
func TestCombineAvailability(t *testing.T) {
	t.Parallel()

	require.Equal(t, AvailabilityUnknown, CombineAvailability())

	// All final -> final.
	require.Equal(
		t, AvailableFinal, CombineAvailability(
			AvailableFinal, AvailableFinal,
		),
	)

	// A provisional parent downgrades a final one.
	require.Equal(
		t, AvailableProvisional, CombineAvailability(
			AvailableFinal, AvailableProvisional,
		),
	)

	// Any limbo dominates available parents.
	require.Equal(
		t, LimboReorg, CombineAvailability(
			AvailableFinal, AvailableProvisional, LimboReorg,
		),
	)

	// Conflict limbo dominates reorg limbo.
	require.Equal(
		t, LimboConflict, CombineAvailability(
			LimboReorg, LimboConflict,
		),
	)

	// Invalidated dominates everything.
	require.Equal(
		t, Invalidated, CombineAvailability(
			AvailableFinal, LimboConflict, Invalidated,
			AvailableProvisional,
		),
	)

	// Unknown dominates available but not limbo/invalidated.
	require.Equal(
		t, AvailabilityUnknown, CombineAvailability(
			AvailableProvisional, AvailabilityUnknown,
		),
	)
	require.Equal(
		t, LimboReorg, CombineAvailability(
			AvailabilityUnknown, LimboReorg,
		),
	)
}

// TestAvailabilityStringStable pins the string names.
func TestAvailabilityStringStable(t *testing.T) {
	t.Parallel()

	require.Equal(t, "available_final", AvailableFinal.String())
	require.Equal(t, "available_provisional", AvailableProvisional.String())
	require.Equal(t, "available_unknown", AvailabilityUnknown.String())
	require.Equal(t, "limbo_reorg", LimboReorg.String())
	require.Equal(t, "limbo_conflict", LimboConflict.String())
	require.Equal(t, "invalidated", Invalidated.String())
}

// TestLineageAvailabilityFromStore exercises the store-driven lineage gate:
// it combines the worst availability across a VTXO's parent batches, treats a
// missing record as unknown (non-blocking), and reports blocking only for
// limbo/invalidated lineage.
func TestLineageAvailabilityFromStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newFakeStore()

	finalTx := chainhash.Hash{0x01}
	reorgTx := chainhash.Hash{0x02}
	conflictTx := chainhash.Hash{0x03}
	missingTx := chainhash.Hash{0x04}

	put := func(txid chainhash.Hash, st State) {
		require.NoError(
			t,
			store.UpsertBatch(
				ctx, &Record{
					BatchTxID: txid,
					State:     st,
				},
			),
		)
	}
	put(finalTx, StateFinalized)
	put(reorgTx, StateReorgedOut)
	put(conflictTx, StateConflictFinalized)

	// Single finalized parent: available, not blocked.
	avail, err := LineageAvailability(ctx, store, finalTx)
	require.NoError(t, err)
	require.Equal(t, AvailableFinal, avail)
	blocked, _, err := LineageBlocked(ctx, store, finalTx)
	require.NoError(t, err)
	require.False(t, blocked)

	// A reorged parent alongside a final one: limbo, blocked.
	avail, err = LineageAvailability(ctx, store, finalTx, reorgTx)
	require.NoError(t, err)
	require.Equal(t, LimboReorg, avail)
	blocked, _, err = LineageBlocked(ctx, store, finalTx, reorgTx)
	require.NoError(t, err)
	require.True(t, blocked)

	// An invalidated parent dominates: blocked.
	blocked, avail, err = LineageBlocked(ctx, store, finalTx, conflictTx)
	require.NoError(t, err)
	require.True(t, blocked)
	require.Equal(t, Invalidated, avail)

	// A missing (unregistered) parent is unknown and does NOT block.
	avail, err = LineageAvailability(ctx, store, finalTx, missingTx)
	require.NoError(t, err)
	require.Equal(t, AvailabilityUnknown, avail)
	blocked, _, err = LineageBlocked(ctx, store, finalTx, missingTx)
	require.NoError(t, err)
	require.False(t, blocked)

	// No parents: unknown, not blocked.
	blocked, _, err = LineageBlocked(ctx, store)
	require.NoError(t, err)
	require.False(t, blocked)
}
