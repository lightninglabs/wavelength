package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestBranchSpendRetainsConflict proves that a foreign spend below the
// commitment root invalidates the lineage without losing the reclaim signal.
func TestBranchSpendRetainsConflict(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.BatchExpiry = 110
	child, _, _, store := newActorHarness(t, proof, desc)
	mustAsk(t, child.Ref(), &StartUnrollRequest{Height: 100})
	branch := wire.OutPoint{Hash: proof.RootTxids()[0]}
	mustAsk(t, child.Ref(), &SpendObservedMsg{
		Outpoint: branch, SpendingTxid: chainhash.Hash{0xee},
		SpendingHeight: 120,
	})
	state, ok := mustAsk(
		t, child.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseFailed, state.Phase)
	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.True(t, checkpoint.Conflicted)
	require.Contains(t, checkpoint.State.ConfirmedTxids, branch.Hash)
}

// TestBranchSpendBeforeKnownExpiryDoesNotAuthorizeReclaim keeps early and
// unclassified foreign spends out of the expired-to-live recovery path.
func TestBranchSpendBeforeKnownExpiryDoesNotAuthorizeReclaim(t *testing.T) {
	for _, expiry := range []int32{0, 130} {
		proof := buildLinearProof(t)
		desc := testDescriptor(
			t, proof.TargetOutpoint(), proof.CSVDelay(),
		)
		desc.BatchExpiry = expiry
		child, _, _, store := newActorHarness(t, proof, desc)
		mustAsk(t, child.Ref(), &StartUnrollRequest{Height: 100})
		mustAsk(t, child.Ref(), &SpendObservedMsg{
			Outpoint:     wire.OutPoint{Hash: proof.RootTxids()[0]},
			SpendingTxid: chainhash.Hash{0xee}, SpendingHeight: 120,
		})
		checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
		require.NotEmpty(t, checkpoint.Fail)
		require.False(t, checkpoint.Conflicted)
	}
}

// TestBranchSpendReplayBeforeResume keeps conflict classification independent
// of in-memory spend watches when a durable spend is the first restored turn.
func TestBranchSpendReplayBeforeResume(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.BatchExpiry = 110
	first, original, _, store := newActorHarness(t, proof, desc)
	mustAsk(t, first.Ref(), &StartUnrollRequest{Height: 100})
	first.Stop()
	restored := &behavior{cfg: original.cfg, log: original.log}
	require.NoError(t, restored.restoreCheckpoint(t.Context()))
	require.Empty(t, restored.proofSpendWatches)
	child := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "replayed-branch-spend",
		Behavior:    adaptTx(restored),
		MailboxSize: 16,
	})
	restored.selfRef = child.TellRef()
	child.Start()
	t.Cleanup(child.Stop)
	mustAsk(t, child.Ref(), &SpendObservedMsg{
		Outpoint:       wire.OutPoint{Hash: proof.RootTxids()[0]},
		SpendingTxid:   chainhash.Hash{0xee},
		SpendingHeight: 120,
	})
	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.True(t, checkpoint.Conflicted)
}
