package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestLineageCommitmentTxids verifies the helper collects the direct
// commitment txid plus every distinct ancestor commitment txid, dedupes, and
// skips the zero hash.
func TestLineageCommitmentTxids(t *testing.T) {
	t.Parallel()

	direct := chainhash.Hash{0x01}
	parentA := chainhash.Hash{0x02}
	parentB := chainhash.Hash{0x03}

	desc := &Descriptor{
		CommitmentTxID: direct,
		Ancestry: []Ancestry{
			{
				CommitmentTxID: parentA,
			},
			{
				CommitmentTxID: parentB,
			},
			// Duplicate of the direct txid: must be deduped.
			{
				CommitmentTxID: direct,
			},
			// Zero hash: must be skipped.
			{
				CommitmentTxID: chainhash.Hash{},
			},
		},
	}

	got := lineageCommitmentTxids(desc)
	require.Equal(
		t, []chainhash.Hash{direct, parentA, parentB}, got,
	)
}

// TestLineageCommitmentTxidsDirectOnly verifies a VTXO with no ancestry (e.g.
// an incoming VTXO materialized without its commitment tree) still yields its
// direct commitment txid so the gate governs it.
func TestLineageCommitmentTxidsDirectOnly(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{CommitmentTxID: chainhash.Hash{0x09}}
	require.Equal(
		t, []chainhash.Hash{{0x09}}, lineageCommitmentTxids(desc),
	)
}

// TestSelectExcludesMultiParentLimboLineage verifies that a cross-commitment
// OOR VTXO is gated out when ANY of its ancestor batches is in limbo, even
// though its direct commitment batch is canonical. This is the multi-parent
// extension: the worst parent dominates.
func TestSelectExcludesMultiParentLimboLineage(t *testing.T) {
	t.Parallel()

	good := makeDescriptor(t, 40000, 0)
	multi := makeDescriptor(t, 50000, 1)

	good.CommitmentTxID = chainhash.Hash{0xaa}

	// multi descends from two batches: its direct commitment (canonical)
	// and a cross-commitment ancestor that reorged out.
	directBatch := chainhash.Hash{0xbb}
	ancestorBatch := chainhash.Hash{0xcc}
	multi.CommitmentTxID = directBatch
	multi.Ancestry = []Ancestry{
		{
			CommitmentTxID: directBatch,
		},
		{
			CommitmentTxID: ancestorBatch,
		},
	}

	mgr, store := newTestManager(t, []*Descriptor{good, multi})
	mgr.cfg.BatchCanonicality = &fakeBatchCanon{
		states: map[chainhash.Hash]batchcanon.State{
			good.CommitmentTxID: batchcanon.StateProvisional,
			directBatch:         batchcanon.StateProvisional,
			ancestorBatch:       batchcanon.StateReorgedOut,
		},
	}

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{good, multi}, nil)
	store.On("GetVTXO", mock.Anything, good.Outpoint).Return(good, nil)
	store.On("GetVTXO", mock.Anything, multi.Outpoint).Return(multi, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok)

	// The 50000 multi-parent candidate is gated out for its reorged-out
	// ANCESTOR batch despite its direct batch being canonical, so selection
	// falls to the 40000 candidate.
	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(t, good.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)
}
