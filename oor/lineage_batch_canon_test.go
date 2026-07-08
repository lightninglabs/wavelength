package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// oorBCRef aliases the canonicality manager tell-ref to keep test literals
// within the line limit.
type oorBCRef = actor.TellOnlyRef[batchcanon.ManagerMsg]

// lineageFragment builds an ancestry fragment anchored at the given commitment
// txid whose tree root carries the supplied batch-output pkScript.
func lineageFragment(txid chainhash.Hash, pkScript []byte) vtxo.Ancestry {
	return vtxo.Ancestry{
		CommitmentTxID: txid,
		TreePath: &tree.Tree{
			BatchOutput: &wire.TxOut{
				PkScript: pkScript,
			},
		},
	}
}

func lineageOutpoint(seed byte) wire.OutPoint {
	var h chainhash.Hash
	h[0] = seed

	return wire.OutPoint{Hash: h, Index: uint32(seed)}
}

// TestRegisterLineageBatchesRegistersDistinctAncestors verifies that receiving
// OOR VTXOs registers one RegisterBatchRequest per distinct ancestor commitment
// tx, carrying the tree-root batch pkScript and the dependent VTXO outpoints,
// and that a batch shared across two received VTXOs accumulates both.
func TestRegisterLineageBatchesRegistersDistinctAncestors(t *testing.T) {
	t.Parallel()

	ref := actor.NewChannelTellOnlyRef[batchcanon.ManagerMsg](
		"oor-batchcanon-test", 8,
	)
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			BatchCanonicality: fn.Some[oorBCRef](ref),
		},
		log: btclog.Disabled,
	}

	txA := chainhash.Hash{0xaa}
	txB := chainhash.Hash{0xbb}
	scriptA := []byte{0x51, 0x20, 0xaa}
	scriptB := []byte{0x51, 0x20, 0xbb}

	vtxo1 := lineageOutpoint(1)
	vtxo2 := lineageOutpoint(2)

	descs := []*vtxo.Descriptor{
		{
			Outpoint: vtxo1,
			Ancestry: []vtxo.Ancestry{
				lineageFragment(txA, scriptA),
			},
		},
		{
			// Multi-parent: shares txA and adds txB.
			Outpoint: vtxo2,
			Ancestry: []vtxo.Ancestry{
				lineageFragment(txA, scriptA),
				lineageFragment(txB, scriptB),
			},
		},
	}

	b.registerLineageBatches(t.Context(), descs)

	got := make(map[chainhash.Hash]*batchcanon.RegisterBatchRequest)
	for range 2 {
		msg, ok := ref.AwaitMessage(time.Second)
		require.True(t, ok, "expected a RegisterBatchRequest")
		req, ok := msg.(*batchcanon.RegisterBatchRequest)
		require.True(t, ok)
		got[req.BatchTxID] = req
	}

	// No third registration.
	_, extra := ref.AwaitMessage(100 * time.Millisecond)
	require.False(t, extra, "expected exactly two distinct batches")

	require.Contains(t, got, txA)
	require.Equal(t, scriptA, got[txA].ConfirmationPkScript)
	require.ElementsMatch(
		t, []wire.OutPoint{vtxo1, vtxo2}, got[txA].DependentVTXOs,
	)

	require.Contains(t, got, txB)
	require.Equal(t, scriptB, got[txB].ConfirmationPkScript)
	require.Equal(t, []wire.OutPoint{vtxo2}, got[txB].DependentVTXOs)
}

// TestRegisterLineageBatchesDormantWhenUnwired verifies registration is a no-op
// when no canonicality manager ref is configured.
func TestRegisterLineageBatchesDormantWhenUnwired(t *testing.T) {
	t.Parallel()

	b := &sessionBehavior{
		cfg: SessionActorConfig{
			BatchCanonicality: fn.None[oorBCRef](),
		},
		log: btclog.Disabled,
	}

	// Must not panic with a populated lineage and no ref.
	b.registerLineageBatches(t.Context(), []*vtxo.Descriptor{
		{
			Outpoint: lineageOutpoint(1),
			Ancestry: []vtxo.Ancestry{
				lineageFragment(
					chainhash.Hash{0xaa}, []byte{0x51},
				),
			},
		},
	})
}

// TestRegisterLineageBatchesSkipsIncompleteFragments verifies fragments with no
// tree path / batch output or a zero txid are skipped (they cannot be watched),
// leaving the gate permissive rather than registering an unwatchable batch.
func TestRegisterLineageBatchesSkipsIncompleteFragments(t *testing.T) {
	t.Parallel()

	ref := actor.NewChannelTellOnlyRef[batchcanon.ManagerMsg](
		"oor-batchcanon-skip", 4,
	)
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			BatchCanonicality: fn.Some[oorBCRef](ref),
		},
		log: btclog.Disabled,
	}

	b.registerLineageBatches(t.Context(), []*vtxo.Descriptor{
		{
			Outpoint: lineageOutpoint(1),
			Ancestry: []vtxo.Ancestry{
				// Nil tree path: skipped.
				{CommitmentTxID: chainhash.Hash{0xaa}},
				// Zero txid: skipped.
				lineageFragment(chainhash.Hash{}, []byte{0x51}),
			},
		},
	})

	_, ok := ref.AwaitMessage(200 * time.Millisecond)
	require.False(t, ok, "no registration expected for incomplete lineage")
}
