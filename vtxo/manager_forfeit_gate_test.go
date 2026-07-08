package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestForfeitLineageBlockedOnLimbo verifies the explicit-outpoint forfeit gate
// refuses a VTXO whose batch reorged out, matching the coin-selection gate so
// the wallet's reserve-by-name paths (refresh/leave/sweep/replay) cannot
// forfeit a VTXO that is off the canonical chain.
func TestForfeitLineageBlockedOnLimbo(t *testing.T) {
	t.Parallel()

	v := makeDescriptor(t, 50_000, 0)
	v.CommitmentTxID = chainhash.Hash{0xaa}

	mgr, store := newTestManager(t, []*Descriptor{v})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			v.CommitmentTxID: batchcanon.StateReorgedOut,
		},
	}
	store.On("GetVTXO", mock.Anything, v.Outpoint).Return(v, nil)

	blocked, avail, err := mgr.forfeitLineageBlocked(
		t.Context(), v.Outpoint,
	)
	require.NoError(t, err)
	require.True(t, blocked)
	require.Equal(t, batchcanon.LimboReorg, avail)
}

// TestForfeitLineageNotBlockedWhenCanonical verifies a canonical VTXO is
// admissible for forfeit.
func TestForfeitLineageNotBlockedWhenCanonical(t *testing.T) {
	t.Parallel()

	v := makeDescriptor(t, 50_000, 0)
	v.CommitmentTxID = chainhash.Hash{0xaa}

	mgr, store := newTestManager(t, []*Descriptor{v})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			v.CommitmentTxID: batchcanon.StateProvisional,
		},
	}
	store.On("GetVTXO", mock.Anything, v.Outpoint).Return(v, nil)

	blocked, _, err := mgr.forfeitLineageBlocked(t.Context(), v.Outpoint)
	require.NoError(t, err)
	require.False(t, blocked)
}

// TestForfeitLineageGateDormantWhenNoStore verifies the forfeit gate is a no-op
// when no canonicality store is wired.
func TestForfeitLineageGateDormantWhenNoStore(t *testing.T) {
	t.Parallel()

	v := makeDescriptor(t, 50_000, 0)
	mgr, _ := newTestManager(t, []*Descriptor{v})

	blocked, _, err := mgr.forfeitLineageBlocked(t.Context(), v.Outpoint)
	require.NoError(t, err)
	require.False(t, blocked)
}
