package tree

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// Test helpers and fixtures.

// newNode is a test helper that creates a simple Node without FinalKey.
// Production code should use NewLeafNode or NewBranchNode instead.
func newNode(input wire.OutPoint, outputs []*wire.TxOut,
	cosigners []*btcec.PublicKey) *Node {

	return &Node{
		Input:     input,
		Outputs:   outputs,
		CoSigners: cosigners,
		Children:  make(map[uint32]*Node),
	}
}

// createTestKey generates a new private/public key pair for testing.
func createTestKey(t *testing.T) (*btcec.PrivateKey, *btcec.PublicKey) {
	t.Helper()
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey, privKey.PubKey()
}

// createTestSignature creates a test schnorr signature.
func createTestSignature(t *testing.T) *schnorr.Signature {
	t.Helper()
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	msg := chainhash.HashH([]byte("test message"))
	sig, err := schnorr.Sign(privKey, msg[:])
	require.NoError(t, err)

	return sig
}

// createSimpleLeaf creates a simple leaf node for testing.
func createSimpleLeaf(name string, value int64,
	cosigners []*btcec.PublicKey) *Node {

	return newNode(
		wire.OutPoint{
			Hash:  chainhash.HashH([]byte(name)),
			Index: 0,
		},
		[]*wire.TxOut{
			{Value: value, PkScript: []byte("vtxo_script")},
			arkscript.AnchorOutput(),
		},
		cosigners,
	)
}

// createTestTree creates a simple test tree:
//
//	    root
//	   /    \
//	leaf1  leaf2
func createTestTree(t *testing.T) (*Node, *Node, *Node, []*btcec.PublicKey) {
	t.Helper()

	_, key1 := createTestKey(t)
	_, key2 := createTestKey(t)
	_, key3 := createTestKey(t)

	leaf1 := createSimpleLeaf("leaf1", 1000, []*btcec.PublicKey{key1})
	leaf2 := createSimpleLeaf("leaf2", 2000, []*btcec.PublicKey{key2})

	root := newNode(
		wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root")),
			Index: 0,
		},
		[]*wire.TxOut{
			{Value: 1000, PkScript: []byte("out0")},
			{Value: 2000, PkScript: []byte("out1")},
			arkscript.AnchorOutput(),
		},
		[]*btcec.PublicKey{key1, key2, key3},
	)
	root.SetChildren(map[uint32]*Node{
		0: leaf1,
		1: leaf2,
	})

	// Fix children to point to correct parent.
	rootTXID, err := root.TXID()
	require.NoError(t, err)
	leaf1.Input = wire.OutPoint{Hash: rootTXID, Index: 0}
	leaf2.Input = wire.OutPoint{Hash: rootTXID, Index: 1}

	return root, leaf1, leaf2, []*btcec.PublicKey{key1, key2, key3}
}

// createDeepTree creates a deeper test tree for depth testing:
//
//	       root
//	      /    \
//	  branch1  branch2
//	  /    \    /    \
//	l1    l2  l3    l4
func createDeepTree(t *testing.T) *Node {
	t.Helper()

	_, key1 := createTestKey(t)
	_, key2 := createTestKey(t)
	_, key3 := createTestKey(t)
	_, key4 := createTestKey(t)

	// Create leaf nodes.
	leaf1 := createSimpleLeaf("leaf1", 1000, []*btcec.PublicKey{key1})
	leaf2 := createSimpleLeaf("leaf2", 2000, []*btcec.PublicKey{key2})
	leaf3 := createSimpleLeaf("leaf3", 3000, []*btcec.PublicKey{key3})
	leaf4 := createSimpleLeaf("leaf4", 4000, []*btcec.PublicKey{key4})

	// Create branch nodes.
	branch1 := newNode(
		wire.OutPoint{
			Hash:  chainhash.HashH([]byte("branch1")),
			Index: 0,
		},
		[]*wire.TxOut{
			{Value: 1000, PkScript: []byte("b1_out0")},
			{Value: 2000, PkScript: []byte("b1_out1")},
			arkscript.AnchorOutput(),
		},
		[]*btcec.PublicKey{key1, key2},
	)
	branch1.SetChildren(map[uint32]*Node{
		0: leaf1,
		1: leaf2,
	})

	branch2 := newNode(
		wire.OutPoint{
			Hash:  chainhash.HashH([]byte("branch2")),
			Index: 1,
		},
		[]*wire.TxOut{
			{Value: 3000, PkScript: []byte("b2_out0")},
			{Value: 4000, PkScript: []byte("b2_out1")},
			arkscript.AnchorOutput(),
		},
		[]*btcec.PublicKey{key3, key4},
	)
	branch2.SetChildren(map[uint32]*Node{
		0: leaf3,
		1: leaf4,
	})

	// Create root node.
	root := newNode(
		wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root")),
			Index: 0,
		},
		[]*wire.TxOut{
			{Value: 3000, PkScript: []byte("r_out0")},
			{Value: 7000, PkScript: []byte("r_out1")},
			arkscript.AnchorOutput(),
		},
		[]*btcec.PublicKey{key1, key2, key3, key4},
	)
	root.SetChildren(map[uint32]*Node{
		0: branch1,
		1: branch2,
	})

	// Fix all the input references.
	rootTXID, _ := root.TXID()
	branch1.Input = wire.OutPoint{Hash: rootTXID, Index: 0}
	branch2.Input = wire.OutPoint{Hash: rootTXID, Index: 1}

	b1TXID, _ := branch1.TXID()
	leaf1.Input = wire.OutPoint{Hash: b1TXID, Index: 0}
	leaf2.Input = wire.OutPoint{Hash: b1TXID, Index: 1}

	b2TXID, _ := branch2.TXID()
	leaf3.Input = wire.OutPoint{Hash: b2TXID, Index: 0}
	leaf4.Input = wire.OutPoint{Hash: b2TXID, Index: 1}

	return root
}

// TestNodeTransactionConversion tests converting nodes to Bitcoin
// transactions in various forms.
func TestNodeTransactionConversion(t *testing.T) {
	t.Parallel()

	t.Run("ToTx converts node to unsigned transaction", func(t *testing.T) {
		t.Parallel()
		node := newNode(
			wire.OutPoint{
				Hash:  chainhash.HashH([]byte("test")),
				Index: 0,
			},
			[]*wire.TxOut{
				{Value: 1000, PkScript: []byte("script1")},
				arkscript.AnchorOutput(),
			},
			[]*btcec.PublicKey{},
		)

		tx, err := node.ToTx()
		require.NoError(t, err)
		require.NotNil(t, tx)

		// Verify transaction properties.
		require.Equal(t, int32(3), tx.Version)
		require.Len(t, tx.TxIn, 1)
		require.Len(t, tx.TxOut, 2)
		require.Equal(t, node.Input, tx.TxIn[0].PreviousOutPoint)
		require.Equal(
			t, wire.MaxTxInSequenceNum, tx.TxIn[0].Sequence,
		)
		require.Equal(t, int64(1000), tx.TxOut[0].Value)
		require.Equal(t, int64(0), tx.TxOut[1].Value)
	})

	t.Run("ToSignedTx converts node with signature", func(t *testing.T) {
		t.Parallel()
		sig := createTestSignature(t)
		node := newNode(
			wire.OutPoint{
				Hash:  chainhash.HashH([]byte("test")),
				Index: 0,
			},
			[]*wire.TxOut{
				{Value: 1000, PkScript: []byte("script1")},
				arkscript.AnchorOutput(),
			},
			[]*btcec.PublicKey{},
		)
		node.AddSignature(sig)

		tx, err := node.ToSignedTx()
		require.NoError(t, err)
		require.NotNil(t, tx)

		// Verify signature is in witness.
		require.Len(t, tx.TxIn, 1)
		require.Len(t, tx.TxIn[0].Witness, 1)
		require.Equal(t, sig.Serialize(), tx.TxIn[0].Witness[0])
	})

	t.Run("ToSignedTx fails without signature", func(t *testing.T) {
		t.Parallel()
		node := newNode(
			wire.OutPoint{
				Hash:  chainhash.HashH([]byte("test")),
				Index: 0,
			},
			[]*wire.TxOut{{Value: 1000}},
			[]*btcec.PublicKey{},
		)

		tx, err := node.ToSignedTx()
		require.Error(t, err)
		require.Nil(t, tx)
		require.Contains(t, err.Error(), "no signature present")
	})

	t.Run("TXID computes transaction hash correctly", func(t *testing.T) {
		t.Parallel()
		node := newNode(
			wire.OutPoint{
				Hash:  chainhash.HashH([]byte("test")),
				Index: 0,
			},
			[]*wire.TxOut{
				{Value: 1000, PkScript: []byte("script1")},
				arkscript.AnchorOutput(),
			},
			[]*btcec.PublicKey{},
		)

		txid, err := node.TXID()
		require.NoError(t, err)
		require.NotEqual(t, chainhash.Hash{}, txid)

		// Verify it matches the actual transaction hash.
		tx, err := node.ToTx()
		require.NoError(t, err)
		require.Equal(t, tx.TxHash(), txid)
	})
}

// TestNodeBasicOperations tests basic node operations like leaf detection.
func TestNodeBasicOperations(t *testing.T) {
	t.Parallel()

	t.Run("IsLeaf detects leaf nodes", func(t *testing.T) {
		t.Parallel()

		t.Run("node with no children is leaf", func(t *testing.T) {
			t.Parallel()
			node := newNode(
				wire.OutPoint{}, []*wire.TxOut{},
				[]*btcec.PublicKey{},
			)
			require.True(t, node.IsLeaf())
		})

		t.Run("node with children is not leaf", func(t *testing.T) {
			t.Parallel()
			node := newNode(
				wire.OutPoint{}, []*wire.TxOut{},
				[]*btcec.PublicKey{},
			)
			node.Children[0] = newNode(
				wire.OutPoint{}, []*wire.TxOut{},
				[]*btcec.PublicKey{},
			)
			require.False(t, node.IsLeaf())
		})

		t.Run("nil children map is leaf", func(t *testing.T) {
			t.Parallel()
			node := &Node{
				Children: nil,
			}
			require.True(t, node.IsLeaf())
		})
	})
}

// TestNodeTreeTraversal tests tree traversal methods.
func TestNodeTreeTraversal(t *testing.T) {
	t.Parallel()

	root, leaf1, leaf2, _ := createTestTree(t)

	t.Run("ForEach visits all nodes", func(t *testing.T) {
		t.Parallel()
		var visited []*Node
		err := root.ForEach(func(n *Node) error {
			visited = append(visited, n)

			return nil
		})
		require.NoError(t, err)
		require.Len(t, visited, 3)
		require.Equal(t, root, visited[0])
		require.Contains(t, visited, leaf1)
		require.Contains(t, visited, leaf2)
	})

	t.Run("ForEach stops on error", func(t *testing.T) {
		t.Parallel()
		visitCount := 0
		err := root.ForEach(func(n *Node) error {
			visitCount++
			if visitCount == 2 {
				return fmt.Errorf("stop here")
			}

			return nil
		})
		require.Error(t, err)
		require.Equal(t, 2, visitCount)
		require.Contains(t, err.Error(), "stop here")
	})

	t.Run("ForEachLeaf visits only leaves", func(t *testing.T) {
		t.Parallel()
		var visited []*Node
		err := root.ForEachLeaf(func(n *Node) error {
			visited = append(visited, n)

			return nil
		})
		require.NoError(t, err)
		require.Len(t, visited, 2)
		require.Contains(t, visited, leaf1)
		require.Contains(t, visited, leaf2)
		require.NotContains(t, visited, root)
	})

	t.Run("ForEachLeaf stops on error", func(t *testing.T) {
		t.Parallel()
		visitCount := 0
		err := root.ForEachLeaf(func(n *Node) error {
			visitCount++

			return fmt.Errorf("leaf error")
		})
		require.Error(t, err)
		require.Equal(t, 1, visitCount)
	})

	t.Run("NodesIter iterates all nodes", func(t *testing.T) {
		t.Parallel()
		var visited []*Node
		for node := range root.NodesIter() {
			visited = append(visited, node)
		}
		require.Len(t, visited, 3)
		require.Equal(t, root, visited[0])
		require.Contains(t, visited, leaf1)
		require.Contains(t, visited, leaf2)
	})

	t.Run("NodesIter can break early", func(t *testing.T) {
		t.Parallel()
		count := 0
		for range root.NodesIter() {
			count++
			if count == 1 {
				break
			}
		}
		require.Equal(t, 1, count)
	})

	t.Run("LeavesIter iterates only leaves", func(t *testing.T) {
		t.Parallel()
		var visited []*Node
		for leaf := range root.LeavesIter() {
			visited = append(visited, leaf)
		}
		require.Len(t, visited, 2)
		require.Contains(t, visited, leaf1)
		require.Contains(t, visited, leaf2)
		require.NotContains(t, visited, root)
	})

	t.Run("LeavesIter can break early", func(t *testing.T) {
		t.Parallel()
		count := 0
		for range root.LeavesIter() {
			count++
			if count == 1 {
				break
			}
		}
		require.Equal(t, 1, count)
	})
}

// TestNodeTreeMetrics tests tree depth and transaction count calculations.
func TestNodeTreeMetrics(t *testing.T) {
	t.Parallel()

	t.Run("single leaf has depth 1 and 1 transaction", func(t *testing.T) {
		t.Parallel()
		node := &Node{Children: make(map[uint32]*Node)}
		require.Equal(t, 1, node.Depth())
		require.Equal(t, 1, node.NumTx())
	})

	t.Run("simple tree metrics", func(t *testing.T) {
		t.Parallel()
		leaf1 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		leaf2 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		root := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		root.Children[0] = leaf1
		root.Children[1] = leaf2

		require.Equal(t, 2, root.Depth())
		require.Equal(t, 3, root.NumTx())
	})

	t.Run("deep tree metrics", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)

		// Tree has 3 levels: root, 2 branches, 4 leaves.
		require.Equal(t, 3, deepRoot.Depth())
		require.Equal(t, 7, deepRoot.NumTx())
	})

	t.Run("unbalanced tree depth", func(t *testing.T) {
		t.Parallel()
		// Create unbalanced tree: root -> branch -> leaf vs leaf.
		leaf1 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		leaf2 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		leaf3 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)

		branch := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		branch.Children[0] = leaf1

		root := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		root.Children[0] = branch
		root.Children[1] = leaf2
		root.Children[2] = leaf3

		// Depth should be 3 (longest path).
		require.Equal(t, 3, root.Depth())
		require.Equal(t, 5, root.NumTx())
	})
}

// TestNodeVerify tests structural verification of the tree.
func TestNodeVerify(t *testing.T) {
	t.Parallel()

	t.Run("valid structure verifies", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)
		err := root.Verify()
		require.NoError(t, err)
	})

	t.Run("wrong child input hash fails", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		badChild := newNode(
			wire.OutPoint{
				Hash:  chainhash.HashH([]byte("wrong")),
				Index: 0,
			},
			[]*wire.TxOut{{Value: 1000}},
			[]*btcec.PublicKey{},
		)

		badRoot := newNode(
			root.Input,
			root.Outputs,
			root.CoSigners,
		)
		badRoot.Children[0] = badChild

		err := badRoot.Verify()
		require.Error(t, err)
		require.Contains(t, err.Error(), "incorrect input")
	})

	t.Run("invalid output index fails", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		badRoot := newNode(
			root.Input,
			root.Outputs,
			root.CoSigners,
		)

		badRootTXID, _ := badRoot.TXID()

		badChild := newNode(
			wire.OutPoint{
				Hash:  badRootTXID,
				Index: 99, // Out of bounds!
			},
			[]*wire.TxOut{{Value: 1000}},
			[]*btcec.PublicKey{},
		)

		badRoot.Children[99] = badChild

		err := badRoot.Verify()
		require.ErrorContains(
			t, err, "child references non-existent output index",
		)
	})

	t.Run("deep tree verifies", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)
		err := deepRoot.Verify()
		require.NoError(t, err)
	})

	t.Run("single child at index 0 verifies", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		singleChildRoot := newNode(
			root.Input,
			[]*wire.TxOut{
				{Value: 1000},
				arkscript.AnchorOutput(),
			},
			root.CoSigners,
		)

		child := createSimpleLeaf("child", 1000, nil)

		singleChildRootTXID, _ := singleChildRoot.TXID()
		child.Input = wire.OutPoint{Hash: singleChildRootTXID, Index: 0}

		singleChildRoot.SetChildren(map[uint32]*Node{
			0: child,
		})

		err := singleChildRoot.Verify()
		require.NoError(t, err)
	})

	t.Run("three sequential children verify", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		threeChildRoot := newNode(
			root.Input,
			[]*wire.TxOut{
				{Value: 1000},
				{Value: 2000},
				{Value: 3000},
				arkscript.AnchorOutput(),
			},
			root.CoSigners,
		)

		child0 := createSimpleLeaf("child0", 1000, nil)
		child1 := createSimpleLeaf("child1", 2000, nil)
		child2 := createSimpleLeaf("child2", 3000, nil)

		threeChildRootTXID, _ := threeChildRoot.TXID()
		child0.Input = wire.OutPoint{Hash: threeChildRootTXID, Index: 0}
		child1.Input = wire.OutPoint{Hash: threeChildRootTXID, Index: 1}
		child2.Input = wire.OutPoint{Hash: threeChildRootTXID, Index: 2}

		threeChildRoot.SetChildren(map[uint32]*Node{
			0: child0,
			1: child1,
			2: child2,
		})

		err := threeChildRoot.Verify()
		require.NoError(t, err)
	})
}

// TestNodeLeafOperations tests operations specific to leaf nodes.
func TestNodeLeafOperations(t *testing.T) {
	t.Parallel()

	root, leaf1, leaf2, keys := createTestTree(t)

	t.Run("GetLeafForCoSigner finds correct leaf", func(t *testing.T) {
		t.Parallel()
		found := root.GetLeafForCoSigner(keys[0])
		require.Equal(t, leaf1, found)

		found = root.GetLeafForCoSigner(keys[1])
		require.Equal(t, leaf2, found)
	})

	t.Run("GetLeafForCoSigner returns nil for non-existent key",
		func(t *testing.T) {
			t.Parallel()
			_, unknownKey := createTestKey(t)
			found := root.GetLeafForCoSigner(unknownKey)
			require.Nil(t, found)
		})

	t.Run("GetNonAnchorOutpoint returns correct outpoint",
		func(t *testing.T) {
			t.Parallel()
			outpoint, err := leaf1.GetNonAnchorOutpoint()
			require.NoError(t, err)
			require.NotNil(t, outpoint)

			expectedTXID, err := leaf1.TXID()
			require.NoError(t, err)

			require.Equal(t, expectedTXID, outpoint.Hash)
			require.Equal(t, uint32(0), outpoint.Index)
		})

	t.Run("GetNonAnchorOutpoint fails for non-leaf", func(t *testing.T) {
		t.Parallel()
		outpoint, err := root.GetNonAnchorOutpoint()
		require.Error(t, err)
		require.Nil(t, outpoint)
		require.Contains(t, err.Error(), "not a leaf")
	})

	t.Run("GetNonAnchorOutpoint fails if no non-anchor output",
		func(t *testing.T) {
			t.Parallel()
			// Create leaf with all anchor outputs.
			badLeaf := newNode(
				wire.OutPoint{},
				[]*wire.TxOut{arkscript.AnchorOutput()},
				[]*btcec.PublicKey{},
			)

			outpoint, err := badLeaf.GetNonAnchorOutpoint()
			require.Error(t, err)
			require.Nil(t, outpoint)
			require.Contains(
				t, err.Error(),
				"no non-anchor output found",
			)
		})
}

// TestExtractPathForCoSigners tests extracting a tree path for a specific
// cosigner.
func TestExtractPathForCoSigners(t *testing.T) {
	t.Parallel()

	t.Run("extracts path for cosigner in simple tree", func(t *testing.T) {
		t.Parallel()
		root, leaf1, _, keys := createTestTree(t)

		// Extract path for key1 (should get root -> leaf1).
		extracted, ok := root.ExtractPathForCoSigners(keys[0])
		require.True(t, ok)
		require.NotNil(t, extracted)

		// Should have same root properties.
		require.Equal(t, root.Input, extracted.Input)
		require.Equal(t, root.CoSigners, extracted.CoSigners)

		// Should have only one child (leaf1).
		require.Len(t, extracted.Children, 1)
		extractedLeaf := extracted.Children[0]
		require.NotNil(t, extractedLeaf)
		require.Equal(t, leaf1.Input, extractedLeaf.Input)

		// Extracted path should remain a valid tree for Verify().
		require.NoError(t, extracted.Verify())
	})

	t.Run("extracts path for cosigner in deep tree", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)

		// Get all leaves to find their cosigners.
		var leaves []*Node
		for leaf := range deepRoot.LeavesIter() {
			leaves = append(leaves, leaf)
		}
		require.Len(t, leaves, 4)

		// Extract path for first leaf's cosigner.
		targetKey := leaves[0].CoSigners[0]
		extracted, ok := deepRoot.ExtractPathForCoSigners(targetKey)
		require.True(t, ok)
		require.NotNil(t, extracted)

		// Verify the extracted path reaches the target leaf.
		foundTarget := false
		for leaf := range extracted.LeavesIter() {
			if ContainsCosigner(leaf.CoSigners, targetKey) {
				foundTarget = true
				break
			}
		}
		require.True(t, foundTarget)
	})

	t.Run("returns nil for non-existent cosigner", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)
		_, unknownKey := createTestKey(t)

		extracted, ok := root.ExtractPathForCoSigners(unknownKey)
		require.False(t, ok)
		require.Nil(t, extracted)
	})

	t.Run("extracted tree has no unrelated branches", func(t *testing.T) {
		t.Parallel()
		root, _, _, keys := createTestTree(t)

		// Extract for key[0] which is only in leaf1.
		extracted, ok := root.ExtractPathForCoSigners(keys[0])
		require.True(t, ok)
		require.NotNil(t, extracted)

		// Should only have 1 leaf in extracted tree.
		leafCount := 0
		for range extracted.LeavesIter() {
			leafCount++
		}
		require.Equal(t, 1, leafCount)
	})
}

// TestExtractPathForIndices tests extracting a tree path by leaf index.
func TestExtractPathForIndices(t *testing.T) {
	t.Parallel()

	t.Run("extracts first leaf", func(t *testing.T) {
		t.Parallel()
		root, leaf1, _, _ := createTestTree(t)

		extracted, err := root.ExtractPathForIndices(0)
		require.NoError(t, err)
		require.NotNil(t, extracted)

		// Should have root with one child.
		require.Equal(t, root.Input, extracted.Input)
		require.Len(t, extracted.Children, 1)

		// Should have leaf1.
		extractedLeaf := extracted.Children[0]
		require.NotNil(t, extractedLeaf)
		require.Equal(t, leaf1.Input, extractedLeaf.Input)
	})

	t.Run("extracts second leaf", func(t *testing.T) {
		t.Parallel()
		root, _, leaf2, _ := createTestTree(t)

		extracted, err := root.ExtractPathForIndices(1)
		require.NoError(t, err)
		require.NotNil(t, extracted)

		// Should have leaf2.
		extractedLeaf := extracted.Children[1]
		require.NotNil(t, extractedLeaf)
		require.Equal(t, leaf2.Input, extractedLeaf.Input)
	})

	t.Run("extracts from deep tree", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)

		// Tree has 4 leaves, try extracting each.
		for i := 0; i < 4; i++ {
			extracted, err := deepRoot.ExtractPathForIndices(i)
			require.NoError(t, err)
			require.NotNil(
				t, extracted, "failed to extract leaf %d", i,
			)

			// Should have exactly one leaf.
			leafCount := 0
			for range extracted.LeavesIter() {
				leafCount++
			}
			require.Equal(t, 1, leafCount)
		}
	})

	t.Run("returns error for out of bounds index", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		extracted, err := root.ExtractPathForIndices(999)
		require.Error(t, err)
		require.Nil(t, extracted)
	})

	t.Run("returns error for negative index", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		extracted, err := root.ExtractPathForIndices(-1)
		require.Error(t, err)
		require.Nil(t, extracted)
		require.Contains(t, err.Error(), "must be non-negative")
	})

	t.Run("single leaf at index 0", func(t *testing.T) {
		t.Parallel()
		leaf := createSimpleLeaf("single", 1000, nil)

		extracted, err := leaf.ExtractPathForIndices(0)
		require.NoError(t, err)
		require.NotNil(t, extracted)
		require.Equal(t, leaf.Input, extracted.Input)
	})
}

// TestPrevOutputFetcher tests creating a PrevOutputFetcher for the tree.
func TestPrevOutputFetcher(t *testing.T) {
	t.Parallel()

	t.Run("creates fetcher for simple tree", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)

		initialOutput := &wire.TxOut{
			Value:    10000,
			PkScript: []byte("initial"),
		}

		fetcher, err := root.PrevOutputFetcher(initialOutput)
		require.NoError(t, err)
		require.NotNil(t, fetcher)

		// Should be able to fetch the initial output.
		fetchedOutput := fetcher.FetchPrevOutput(root.Input)
		require.Equal(t, initialOutput, fetchedOutput)
	})

	t.Run("fetcher contains all tree outputs", func(t *testing.T) {
		t.Parallel()
		root, leaf1, leaf2, _ := createTestTree(t)

		initialOutput := &wire.TxOut{Value: 10000}
		fetcher, err := root.PrevOutputFetcher(initialOutput)
		require.NoError(t, err)

		// Should have outputs for root's children.
		rootTXID, _ := root.TXID()
		out0 := fetcher.FetchPrevOutput(wire.OutPoint{
			Hash:  rootTXID,
			Index: 0,
		})
		require.NotNil(t, out0)
		require.Equal(t, int64(1000), out0.Value)

		// Should have outputs for leaves.
		leaf1TXID, _ := leaf1.TXID()
		leaf1Out := fetcher.FetchPrevOutput(wire.OutPoint{
			Hash:  leaf1TXID,
			Index: 0,
		})
		require.NotNil(t, leaf1Out)

		leaf2TXID, _ := leaf2.TXID()
		leaf2Out := fetcher.FetchPrevOutput(wire.OutPoint{
			Hash:  leaf2TXID,
			Index: 0,
		})
		require.NotNil(t, leaf2Out)
	})

	t.Run("works with deep tree", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)

		initialOutput := &wire.TxOut{Value: 50000}
		fetcher, err := deepRoot.PrevOutputFetcher(initialOutput)
		require.NoError(t, err)
		require.NotNil(t, fetcher)

		// Count how many outputs we can fetch.
		// Should be able to fetch outputs from all 7 transactions.
		// Deep tree: 1 root (3 outputs) + 2 branches (3 each) +
		// 4 leaves (2 each) = 3 + 6 + 8 = 17 outputs.
		outputCount := 0
		for node := range deepRoot.NodesIter() {
			txid, _ := node.TXID()
			for i := range node.Outputs {
				outpoint := wire.OutPoint{
					Hash:  txid,
					Index: uint32(i),
				}
				if fetcher.FetchPrevOutput(outpoint) != nil {
					outputCount++
				}
			}
		}
		require.Equal(t, 17, outputCount)
	})
}

// TestContainsCosigner tests the cosigner membership check.
func TestContainsCosigner(t *testing.T) {
	t.Parallel()

	_, key1 := createTestKey(t)
	_, key2 := createTestKey(t)
	_, key3 := createTestKey(t)

	cosigners := []*btcec.PublicKey{key1, key2}

	t.Run("key present", func(t *testing.T) {
		t.Parallel()
		require.True(t, ContainsCosigner(cosigners, key1))
		require.True(t, ContainsCosigner(cosigners, key2))
	})

	t.Run("key not present", func(t *testing.T) {
		t.Parallel()
		require.False(t, ContainsCosigner(cosigners, key3))
	})

	t.Run("empty cosigners", func(t *testing.T) {
		t.Parallel()
		require.False(
			t,
			ContainsCosigner(
				[]*btcec.PublicKey{}, key1,
			),
		)
	})

	t.Run("nil cosigners", func(t *testing.T) {
		t.Parallel()
		require.False(t, ContainsCosigner(nil, key1))
	})
}

// TestNodeConstructors tests the various Node constructor functions.
func TestNodeConstructors(t *testing.T) {
	t.Parallel()

	t.Run("NewNode creates node with empty children", func(t *testing.T) {
		t.Parallel()
		input := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test")),
			Index: 0,
		}
		outputs := []*wire.TxOut{
			{
				Value:    1000,
				PkScript: []byte("script"),
			},
			arkscript.AnchorOutput(),
		}
		_, key1 := createTestKey(t)
		cosigners := []*btcec.PublicKey{key1}

		node := newNode(input, outputs, cosigners)

		require.Equal(t, input, node.Input)
		require.Equal(t, outputs, node.Outputs)
		require.Equal(t, cosigners, node.CoSigners)
		require.NotNil(t, node.Children)
		require.Empty(t, node.Children)
		require.Nil(t, node.Signature)
	})

	t.Run("NewLeafNode creates valid leaf node", func(t *testing.T) {
		t.Parallel()
		_, ownerKey := createTestKey(t)
		_, operatorKey := createTestKey(t)

		leaf := LeafDescriptor{
			PkScript:    []byte("vtxo_script"),
			Amount:      1000,
			CoSignerKey: ownerKey,
		}

		input := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("parent")),
			Index: 0,
		}

		sweepRoot := make([]byte, 32)
		node, err := NewLeafNode(input, leaf, operatorKey, sweepRoot)
		require.NoError(t, err)
		require.NotNil(t, node)

		// Verify structure.
		require.Equal(t, input, node.Input)
		require.Len(t, node.Outputs, 2)
		require.Equal(t, int64(1000), node.Outputs[0].Value)
		require.Equal(t, leaf.PkScript, node.Outputs[0].PkScript)
		require.Equal(t, int64(0), node.Outputs[1].Value)

		// Verify cosigners (owner and operator).
		// Note: musig2.AggregateKeys sorts keys, so we can't
		// assume order.
		require.Len(t, node.CoSigners, 2)
		require.Contains(t, node.CoSigners, ownerKey)
		require.Contains(t, node.CoSigners, operatorKey)

		// Verify it's a leaf.
		require.True(t, node.IsLeaf())

		// Verify FinalKey is set.
		require.NotNil(t, node.FinalKey)
	})

	t.Run("NewBranchNode creates valid branch with single group",
		func(t *testing.T) {
			t.Parallel()
			_, key1 := createTestKey(t)
			_, key2 := createTestKey(t)
			_, operatorKey := createTestKey(t)

			leaves := []LeafDescriptor{
				{
					PkScript:    []byte("script1"),
					Amount:      1000,
					CoSignerKey: key1,
				},
				{
					PkScript:    []byte("script2"),
					Amount:      2000,
					CoSignerKey: key2,
				},
			}

			input := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("root")),
				Index: 0,
			}

			groups := [][]LeafDescriptor{leaves}
			sweepRoot := make([]byte, 32)

			node, err := NewBranchNode(
				input, groups, operatorKey, sweepRoot,
			)
			require.NoError(t, err)
			require.NotNil(t, node)

			// Verify structure.
			require.Equal(t, input, node.Input)
			// Should have 1 group output + 1 anchor.
			require.Len(t, node.Outputs, 2)
			// Combined amount of leaves in group.
			require.Equal(t, int64(3000), node.Outputs[0].Value)
			require.Equal(t, int64(0), node.Outputs[1].Value)

			// Verify cosigners include operator and both leaf keys.
			require.Len(t, node.CoSigners, 3)
			require.Contains(t, node.CoSigners, operatorKey)
			require.Contains(t, node.CoSigners, key1)
			require.Contains(t, node.CoSigners, key2)

			// Verify it's not a leaf (has no children yet).
			require.True(t, node.IsLeaf())
		})

	t.Run("NewBranchNode creates valid branch with multiple groups",
		func(t *testing.T) {
			t.Parallel()
			_, key1 := createTestKey(t)
			_, key2 := createTestKey(t)
			_, key3 := createTestKey(t)
			_, operatorKey := createTestKey(t)

			group1 := []LeafDescriptor{
				{
					PkScript:    []byte("script1"),
					Amount:      1000,
					CoSignerKey: key1,
				},
				{
					PkScript:    []byte("script2"),
					Amount:      2000,
					CoSignerKey: key2,
				},
			}

			group2 := []LeafDescriptor{
				{
					PkScript:    []byte("script3"),
					Amount:      3000,
					CoSignerKey: key3,
				},
			}

			input := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("root")),
				Index: 0,
			}

			groups := [][]LeafDescriptor{group1, group2}
			sweepRoot := make([]byte, 32)

			node, err := NewBranchNode(
				input, groups, operatorKey, sweepRoot,
			)
			require.NoError(t, err)
			require.NotNil(t, node)

			// Should have 2 group outputs + 1 anchor.
			require.Len(t, node.Outputs, 3)
			require.Equal(t, int64(3000), node.Outputs[0].Value)
			require.Equal(t, int64(3000), node.Outputs[1].Value)
			require.Equal(t, int64(0), node.Outputs[2].Value)

			// Verify all cosigners are present.
			require.Len(t, node.CoSigners, 4)
			require.Contains(t, node.CoSigners, operatorKey)
			require.Contains(t, node.CoSigners, key1)
			require.Contains(t, node.CoSigners, key2)
			require.Contains(t, node.CoSigners, key3)
		})

	t.Run("NewBranchNode deduplicates cosigners", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, operatorKey := createTestKey(t)

		// Create two leaves with the same cosigner key.
		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script1"),
				Amount:      1000,
				CoSignerKey: key1,
			},
			{
				PkScript:    []byte("script2"),
				Amount:      2000,
				CoSignerKey: key1, // Same key!
			},
		}

		input := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root")),
			Index: 0,
		}

		groups := [][]LeafDescriptor{leaves}
		sweepRoot := make([]byte, 32)

		node, err := NewBranchNode(
			input, groups, operatorKey, sweepRoot,
		)
		require.NoError(t, err)
		require.NotNil(t, node)

		// Should only have operator and key1 (deduplicated).
		require.Len(t, node.CoSigners, 2)
		require.Contains(t, node.CoSigners, operatorKey)
		require.Contains(t, node.CoSigners, key1)
	})
}

// TestSetChildren tests the SetChildren method.
func TestSetChildren(t *testing.T) {
	t.Parallel()

	t.Run("SetChildren replaces existing children", func(t *testing.T) {
		t.Parallel()
		node := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)

		// Initially empty.
		require.Empty(t, node.Children)

		child1 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		child2 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)

		// Set children.
		children := map[uint32]*Node{
			0: child1,
			1: child2,
		}
		node.SetChildren(children)

		require.Len(t, node.Children, 2)
		require.Equal(t, child1, node.Children[0])
		require.Equal(t, child2, node.Children[1])

		// Replace with different children.
		child3 := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		newChildren := map[uint32]*Node{
			5: child3,
		}
		node.SetChildren(newChildren)

		require.Len(t, node.Children, 1)
		require.Equal(t, child3, node.Children[5])
		require.Nil(t, node.Children[0])
		require.Nil(t, node.Children[1])
	})

	t.Run("SetChildren can clear children", func(t *testing.T) {
		t.Parallel()
		node := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)

		child := newNode(
			wire.OutPoint{}, []*wire.TxOut{}, []*btcec.PublicKey{},
		)
		node.Children[0] = child

		// Clear by setting empty map.
		node.SetChildren(make(map[uint32]*Node))
		require.Empty(t, node.Children)
	})
}

// TestUniqueCosigners tests the UniqueCosigners helper function.
func TestUniqueCosigners(t *testing.T) {
	t.Parallel()

	t.Run("removes duplicate keys", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)
		_, key3 := createTestKey(t)

		cosigners := []*btcec.PublicKey{
			key1, key2, key1, key3, key2,
		}

		unique := UniqueCosigners(cosigners)

		require.Len(t, unique, 3)
		require.Contains(t, unique, key1)
		require.Contains(t, unique, key2)
		require.Contains(t, unique, key3)
	})

	t.Run("preserves order", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)
		_, key3 := createTestKey(t)

		cosigners := []*btcec.PublicKey{
			key3, key1, key2, key1, key3,
		}

		unique := UniqueCosigners(cosigners)

		// Should preserve first occurrence order.
		require.Len(t, unique, 3)
		require.Equal(t, key3, unique[0])
		require.Equal(t, key1, unique[1])
		require.Equal(t, key2, unique[2])
	})

	t.Run("handles empty list", func(t *testing.T) {
		t.Parallel()
		unique := UniqueCosigners([]*btcec.PublicKey{})
		require.Empty(t, unique)
	})

	t.Run("handles nil", func(t *testing.T) {
		t.Parallel()
		unique := UniqueCosigners(nil)
		require.Empty(t, unique)
	})

	t.Run("handles single key", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)

		unique := UniqueCosigners([]*btcec.PublicKey{key1})
		require.Len(t, unique, 1)
		require.Equal(t, key1, unique[0])
	})

	t.Run("handles all unique keys", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)
		_, key3 := createTestKey(t)

		cosigners := []*btcec.PublicKey{key1, key2, key3}
		unique := UniqueCosigners(cosigners)

		require.Len(t, unique, 3)
		require.Equal(t, cosigners, unique)
	})
}

// TestComputeFinalKey tests the ComputeFinalKey helper function.
func TestComputeFinalKey(t *testing.T) {
	t.Parallel()

	t.Run("ComputeFinalKey works for multi-key", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)

		sweepRoot := make([]byte, 32)
		finalKey, err := ComputeFinalKey(
			[]*btcec.PublicKey{key1, key2}, sweepRoot,
		)
		require.NoError(t, err)
		require.NotNil(t, finalKey)
	})

	t.Run("ComputeFinalKey works for single-key", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)

		sweepRoot := make([]byte, 32)
		finalKey, err := ComputeFinalKey(
			[]*btcec.PublicKey{key1}, sweepRoot,
		)
		require.NoError(t, err)
		require.NotNil(t, finalKey)
	})

	t.Run("ComputeFinalKey rejects empty cosigners", func(t *testing.T) {
		t.Parallel()
		sweepRoot := make([]byte, 32)
		finalKey, err := ComputeFinalKey(
			[]*btcec.PublicKey{}, sweepRoot,
		)
		require.Error(t, err)
		require.Nil(t, finalKey)
		require.Contains(t, err.Error(), "no cosigners")
	})

	t.Run("ComputeFinalKey matches MuSig2 aggregation", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)

		sweepRoot := make([]byte, 32)

		// Compute using MuSig2 directly.
		aggKey, _, _, err := musig2.AggregateKeys(
			[]*btcec.PublicKey{key1, key2}, true,
			musig2.WithTaprootKeyTweak(sweepRoot),
		)
		require.NoError(t, err)
		expectedKey := aggKey.FinalKey

		// Compute using ComputeFinalKey helper.
		finalKey, err := ComputeFinalKey(
			[]*btcec.PublicKey{key1, key2}, sweepRoot,
		)
		require.NoError(t, err)

		// Should match.
		require.True(t, expectedKey.IsEqual(finalKey))
	})
}

// TestPrettyPrint tests the tree pretty printing functionality.
func TestPrettyPrint(t *testing.T) {
	t.Parallel()

	t.Run("prints single node", func(t *testing.T) {
		t.Parallel()
		leaf := createSimpleLeaf("single", 1000, nil)
		output := leaf.PrettyPrint()

		require.Contains(t, output, "Transaction Tree")
		require.Contains(t, output, "Leaf")
		require.Contains(t, output, "1000 sats")
	})

	t.Run("prints simple tree structure", func(t *testing.T) {
		t.Parallel()
		root, _, _, _ := createTestTree(t)
		output := root.PrettyPrint()

		require.Contains(t, output, "Transaction Tree")
		require.Contains(t, output, "Branch")
		require.Contains(t, output, "Leaf")

		// Should show amounts.
		require.Contains(t, output, "1000 sats")
		require.Contains(t, output, "2000 sats")
		require.Contains(t, output, "3000 sats") // Root total.
	})

	t.Run("prints deep tree", func(t *testing.T) {
		t.Parallel()
		deepRoot := createDeepTree(t)
		output := deepRoot.PrettyPrint()

		// Count branches and leaves in output.
		require.Contains(t, output, "Branch")
		require.Contains(t, output, "Leaf")

		// Should have tree connectors.
		require.Contains(t, output, "├──")
		require.Contains(t, output, "└──")
	})

	t.Run("includes cosigner aliases", func(t *testing.T) {
		t.Parallel()
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)

		leaf := createSimpleLeaf(
			"leaf", 1000, []*btcec.PublicKey{key1, key2},
		)
		output := leaf.PrettyPrint()

		// Should have key aliases.
		require.Contains(t, output, "K0")
		require.Contains(t, output, "K1")
	})
}
