package tree

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestAssetContextBasicCRUD tests basic create, read, update, delete semantics.
func TestAssetContextBasicCRUD(t *testing.T) {
	t.Parallel()

	ctx := NewAssetContext()
	require.NotNil(t, ctx)
	require.Equal(t, 0, ctx.Len())

	// Create a test node.
	node := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test-input")),
			Index: 0,
		},
	}

	// Get on empty context returns nil.
	require.Nil(t, ctx.Get(node))
	require.Nil(t, ctx.GetProof(node))
	require.Nil(t, ctx.GetTweak(node))
	require.Nil(t, ctx.GetLeaf(node))
	require.Nil(t, ctx.GetLeafPkScript(node))

	// Set state for node.
	testProof := []byte("test-proof-bytes")
	testTweak := []byte("test-tweak-32-bytes-here-xxxxx")
	testPkScript := []byte("test-pkscript")
	testLeaf := &AssetLeafMetadata{
		Funding: 10000,
	}

	state := &AssetNodeState{
		AssetProof:   testProof,
		TaprootTweak: testTweak,
		LeafPkScript: testPkScript,
		Leaf:         testLeaf,
	}
	ctx.Set(node, state)

	require.Equal(t, 1, ctx.Len())

	// Verify retrieval.
	retrieved := ctx.Get(node)
	require.NotNil(t, retrieved)
	require.Equal(t, testProof, retrieved.AssetProof)
	require.Equal(t, testTweak, retrieved.TaprootTweak)
	require.Equal(t, testPkScript, retrieved.LeafPkScript)
	require.Equal(t, testLeaf, retrieved.Leaf)

	// Verify convenience getters.
	require.Equal(t, testProof, ctx.GetProof(node))
	require.Equal(t, testTweak, ctx.GetTweak(node))
	require.Equal(t, testPkScript, ctx.GetLeafPkScript(node))
	require.Equal(t, testLeaf, ctx.GetLeaf(node))

	// Update state (replace).
	newProof := []byte("new-proof-bytes")
	newState := &AssetNodeState{
		AssetProof: newProof,
	}
	ctx.Set(node, newState)

	require.Equal(t, 1, ctx.Len())
	require.Equal(t, newProof, ctx.GetProof(node))
	require.Nil(t, ctx.GetTweak(node))
}

// TestAssetContextNilHandling tests nil-safety of context methods.
func TestAssetContextNilHandling(t *testing.T) {
	t.Parallel()

	// Nil context should handle all operations gracefully.
	var nilCtx *AssetContext

	require.Equal(t, 0, nilCtx.Len())
	require.Nil(t, nilCtx.Get(&Node{}))
	require.Nil(t, nilCtx.GetProof(&Node{}))
	require.Nil(t, nilCtx.GetTweak(&Node{}))
	require.Nil(t, nilCtx.GetLeaf(&Node{}))
	require.Nil(t, nilCtx.GetLeafPkScript(&Node{}))
	require.Nil(t, nilCtx.GetTweakByOutpoint(&Node{}))

	// Set should be no-op on nil asset context (no panic).
	nilCtx.Set(&Node{}, &AssetNodeState{})
	nilCtx.SetTweakByOutpoint(&Node{}, []byte("tweak"))

	// Non-nil asset context with nil node should return nil.
	ctx := NewAssetContext()
	require.Nil(t, ctx.Get(nil))
	require.Nil(t, ctx.GetProof(nil))
	require.Nil(t, ctx.GetTweak(nil))
	require.Nil(t, ctx.GetLeaf(nil))
	require.Nil(t, ctx.GetLeafPkScript(nil))
	require.Nil(t, ctx.GetTweakByOutpoint(nil))

	// Set with nil node or state should be no-op.
	ctx.Set(nil, &AssetNodeState{})
	ctx.Set(&Node{}, nil)
	ctx.SetTweakByOutpoint(nil, []byte("tweak"))
	ctx.SetTweakByOutpoint(&Node{}, nil)
	ctx.SetTweakByOutpoint(&Node{}, []byte{})

	require.Equal(t, 0, ctx.Len())
}

// TestAssetContextOutpointKeyedTweak tests the outpoint-keyed tweak lookup,
// which is critical for ExtractPathForCoSigner compatibility.
func TestAssetContextOutpointKeyedTweak(t *testing.T) {
	t.Parallel()

	ctx := NewAssetContext()

	// Create an original node with a specific outpoint.
	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("original-tx")),
		Index: 1,
	}
	originalNode := &Node{Input: outpoint}

	// Store tweak by outpoint.
	testTweak := []byte("tweak-32-bytes-for-musig2-xxxx")
	ctx.SetTweakByOutpoint(originalNode, testTweak)

	// Verify retrieval via original node.
	require.Equal(t, testTweak, ctx.GetTweakByOutpoint(originalNode))

	// Create a NEW node object with the SAME outpoint (simulating
	// ExtractPathForCoSigner behavior).
	extractedNode := &Node{Input: outpoint}

	// Verify that the extracted node (different pointer, same outpoint)
	// can retrieve the same tweak.
	require.Equal(t, testTweak, ctx.GetTweakByOutpoint(extractedNode))

	// Verify that pointer-keyed lookup does NOT work for extracted node.
	// This demonstrates why outpoint-keyed lookup is necessary.
	require.Nil(t, ctx.GetTweak(extractedNode))
}

// TestTweakLookupFromAssetContext tests the TweakLookupFromAssetContext helper.
func TestTweakLookupFromAssetContext(t *testing.T) {
	t.Parallel()

	// Nil asset context should return a lookup function that always returns
	// nil.
	nilLookup := TweakLookupFromAssetContext(nil)
	require.NotNil(t, nilLookup)
	require.Nil(t, nilLookup(&Node{}))
	require.Nil(t, nilLookup(nil))

	// Create asset context with tweak data.
	assetCtx := NewAssetContext()
	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("test-tx")),
		Index: 0,
	}
	node := &Node{Input: outpoint}
	testTweak := []byte("test-tweak-for-lookup-function")
	assetCtx.SetTweakByOutpoint(node, testTweak)

	// Create lookup function.
	lookup := TweakLookupFromAssetContext(assetCtx)
	require.NotNil(t, lookup)

	// Lookup should return tweak for node with matching outpoint.
	require.Equal(t, testTweak, lookup(node))

	// Lookup should work for different node object with same outpoint.
	otherNode := &Node{Input: outpoint}
	require.Equal(t, testTweak, lookup(otherNode))

	// Lookup should return nil for unknown outpoint.
	unknownNode := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("unknown-tx")),
			Index: 0,
		},
	}
	require.Nil(t, lookup(unknownNode))

	// Lookup should handle nil node gracefully.
	require.Nil(t, lookup(nil))
}

// TestAssetContextMultipleNodes tests context with multiple nodes.
func TestAssetContextMultipleNodes(t *testing.T) {
	t.Parallel()

	ctx2 := NewAssetContext()

	// Create multiple nodes.
	node1 := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("tx1")),
			Index: 0,
		},
	}
	node2 := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("tx2")),
			Index: 0,
		},
	}
	node3 := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("tx3")),
			Index: 1,
		},
	}

	// Set state for each node.
	state1 := &AssetNodeState{AssetProof: []byte("proof1")}
	state2 := &AssetNodeState{AssetProof: []byte("proof2")}
	state3 := &AssetNodeState{AssetProof: []byte("proof3")}

	ctx2.Set(node1, state1)
	ctx2.Set(node2, state2)
	ctx2.Set(node3, state3)

	require.Equal(t, 3, ctx2.Len())

	// Verify each node has correct state.
	require.True(t, bytes.Equal([]byte("proof1"), ctx2.GetProof(node1)))
	require.True(t, bytes.Equal([]byte("proof2"), ctx2.GetProof(node2)))
	require.True(t, bytes.Equal([]byte("proof3"), ctx2.GetProof(node3)))

	// Set tweaks by outpoint for each node.
	ctx2.SetTweakByOutpoint(node1, []byte("tweak1"))
	ctx2.SetTweakByOutpoint(node2, []byte("tweak2"))
	ctx2.SetTweakByOutpoint(node3, []byte("tweak3"))

	// Verify tweak lookup.
	lookup := TweakLookupFromAssetContext(ctx2)
	require.True(t, bytes.Equal([]byte("tweak1"), lookup(node1)))
	require.True(t, bytes.Equal([]byte("tweak2"), lookup(node2)))
	require.True(t, bytes.Equal([]byte("tweak3"), lookup(node3)))
}

// TestGetLeafFunding tests the helper function for leaf funding.
func TestGetLeafFunding(t *testing.T) {
	t.Parallel()

	ctx := NewAssetContext()

	// Create a leaf node.
	leafNode := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("leaf-tx")),
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 10000, PkScript: []byte("vtxo")},
			{Value: 0, PkScript: []byte("anchor")},
		},
	}

	// Create a branch node.
	branchNode := &Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("branch-tx")),
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 10000},
			{Value: 10000},
			{Value: 0},
		},
		Children: map[uint32]*Node{
			0: leafNode,
		},
	}

	// Without asset context state, should return zero funding.
	funding := GetLeafFunding(leafNode, ctx)
	require.Equal(t, btcutil.Amount(0), funding)

	// With asset context state, should return funding.
	testFunding := btcutil.Amount(50000)
	state := &AssetNodeState{
		Leaf: &AssetLeafMetadata{
			Funding: testFunding,
		},
	}
	ctx.Set(leafNode, state)

	funding = GetLeafFunding(leafNode, ctx)
	require.Equal(t, testFunding, funding)

	// Branch node should return zero funding even with state.
	branchState := &AssetNodeState{
		Leaf: &AssetLeafMetadata{
			Funding: 99999,
		},
	}
	ctx.Set(branchNode, branchState)

	funding = GetLeafFunding(branchNode, ctx)
	require.Equal(t, btcutil.Amount(0), funding)

	// Nil inputs should return zero funding.
	funding = GetLeafFunding(nil, ctx)
	require.Equal(t, btcutil.Amount(0), funding)

	funding = GetLeafFunding(leafNode, nil)
	require.Equal(t, btcutil.Amount(0), funding)
}
