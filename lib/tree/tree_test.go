package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// sumLeafAmounts returns the sum of amounts from the provided leaves.
func sumLeafAmounts(leaves []LeafDescriptor) int64 {
	var total int64
	for _, leaf := range leaves {
		total += int64(leaf.Amount)
	}

	return total
}

// TestNewTree tests the NewTree constructor.
func TestNewTree(t *testing.T) {
	t.Parallel()

	t.Run("creates valid tree with single leaf", func(t *testing.T) {
		t.Parallel()
		_, ownerKey := createTestKey(t)
		_, operatorKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("vtxo_script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}
		sweepRoot := make([]byte, 32)

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			sweepRoot, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)
		require.NotNil(t, tree.Root)
		require.Equal(t, rootOutpoint, tree.BatchOutpoint)
		require.Equal(t, rootOutput, tree.BatchOutput)
		require.Equal(t, sweepRoot, tree.SweepTapscriptRoot)

		// Single leaf means root is the leaf.
		require.True(t, tree.Root.IsLeaf())
	})

	t.Run("creates binary tree with multiple leaves", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 4 leaves - should form binary tree.
		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}
		sweepRoot := make([]byte, 32)

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			sweepRoot, 2, // Binary tree
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Binary tree with 4 leaves has depth 3.
		require.Equal(t, 3, tree.Depth())
		require.Equal(t, 4, tree.NumLeaves())
		// Total: 1 root + 2 branches + 4 leaves = 7.
		require.Equal(t, 7, tree.NumTx())
	})

	t.Run("creates quad-tree with higher radix", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 8 leaves with radix 4.
		leaves := make([]LeafDescriptor, 8)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}
		sweepRoot := make([]byte, 32)

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			sweepRoot, 4, // Quad-tree
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Quad-tree with 8 leaves has depth 3.
		// Root splits into 4 branches, each with 2 leaves.
		require.Equal(t, 3, tree.Depth())
		require.Equal(t, 8, tree.NumLeaves())
		// Total: 1 root + 4 branches + 8 leaves = 13.
		require.Equal(t, 13, tree.NumTx())
	})

	t.Run("rejects nil root output", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, nil, leaves, operatorKey,
			make([]byte, 32), 2,
		)
		require.Error(t, err)
		require.Nil(t, tree)
		require.Contains(t, err.Error(), "root output cannot be nil")
	})

	t.Run("rejects nil operator key", func(t *testing.T) {
		t.Parallel()
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: 1000,
			},
			leaves,
			nil,
			make([]byte, 32),
			2,
		)
		require.Error(t, err)
		require.Nil(t, tree)
		require.Contains(t, err.Error(), "operator key cannot be nil")
	})

	t.Run("rejects empty leaves", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: 1000,
			},
			[]LeafDescriptor{},
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.Error(t, err)
		require.Nil(t, tree)
		require.Contains(t, err.Error(), "at least one leaf required")
	})

	t.Run("rejects radix less than 2", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{Value: 1000}, leaves,
			operatorKey, make([]byte, 32), 1, // Invalid radix!
		)
		require.Error(t, err)
		require.Nil(t, tree)
		require.Contains(t, err.Error(), "radix must be at least 2")
	})
}

// TestTreeVerify tests tree verification.
func TestTreeVerify(t *testing.T) {
	t.Parallel()

	t.Run("valid tree verifies", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			make([]byte, 32), 2,
		)
		require.NoError(t, err)

		err = tree.Verify()
		require.NoError(t, err)
	})

	t.Run("rejects mismatched root input", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			make([]byte, 32), 2,
		)
		require.NoError(t, err)

		// Modify batch outpoint to cause mismatch.
		tree.BatchOutpoint = wire.OutPoint{
			Hash:  chainhash.HashH([]byte("wrong")),
			Index: 0,
		}

		err = tree.Verify()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"root input does not match batch outpoint",
		)
	})
}

// TestTreeExtractPathForCoSigners tests extracting tree paths for cosigners.
func TestTreeExtractPathForCoSigners(t *testing.T) {
	t.Parallel()

	t.Run("extracts path for cosigner", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)

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

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			make([]byte, 32), 2,
		)
		require.NoError(t, err)

		// Extract for key1.
		extracted, err := tree.ExtractPathForCoSigners(key1)
		require.NoError(t, err)
		require.NotNil(t, extracted)

		// Should have same batch context.
		require.Equal(t, tree.BatchOutpoint, extracted.BatchOutpoint)
		require.Equal(t, tree.BatchOutput, extracted.BatchOutput)
		require.Equal(
			t, tree.SweepTapscriptRoot,
			extracted.SweepTapscriptRoot,
		)

		// Should have only 1 leaf.
		require.Equal(t, 1, extracted.NumLeaves())
	})

	t.Run("returns nil for non-existent cosigner", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)
		_, unknownKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		extracted, err := tree.ExtractPathForCoSigners(unknownKey)
		require.NoError(t, err)
		require.Nil(t, extracted)
	})

	t.Run("extracts path for multiple cosigners", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)
		_, key3 := createTestKey(t)

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
			{
				PkScript:    []byte("script3"),
				Amount:      3000,
				CoSignerKey: key3,
			},
		}

		rootOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("batch")),
			Index: 0,
		}
		rootOutput := &wire.TxOut{Value: sumLeafAmounts(leaves)}

		tree, err := NewTree(
			rootOutpoint, rootOutput, leaves, operatorKey,
			make([]byte, 32), 2,
		)
		require.NoError(t, err)

		// Extract for both key1 and key2 at once.
		extracted, err := tree.ExtractPathForCoSigners(key1, key2)
		require.NoError(t, err)
		require.NotNil(t, extracted)

		// Should have same batch context.
		require.Equal(t, tree.BatchOutpoint, extracted.BatchOutpoint)
		require.Equal(t, tree.BatchOutput, extracted.BatchOutput)
		require.Equal(
			t, tree.SweepTapscriptRoot,
			extracted.SweepTapscriptRoot,
		)

		// Should have 2 leaves (key1 and key2, but not key3).
		require.Equal(t, 2, extracted.NumLeaves())

		// Verify both paths are valid.
		err = extracted.VerifyVTXOPath(key1, []byte("script1"))
		require.NoError(t, err)

		err = extracted.VerifyVTXOPath(key2, []byte("script2"))
		require.NoError(t, err)

		// key3 should not be in the extracted tree.
		err = extracted.VerifyVTXOPath(key3, []byte("script3"))
		require.Error(t, err)
	})

	t.Run("rejects nil target key", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		_, err = tree.ExtractPathForCoSigners(nil)
		require.ErrorContains(t, err, "target key cannot be nil")
	})

	t.Run("rejects empty target keys", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		_, err = tree.ExtractPathForCoSigners()
		require.ErrorContains(
			t, err, "at least one target key required",
		)
	})
}

// TestTreeExtractPathForIndices tests extracting tree paths by index.
func TestTreeExtractPathForIndices(t *testing.T) {
	t.Parallel()

	t.Run("extracts path for each leaf index", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 4 leaves.
		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000 * (i + 1)),
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Extract each leaf by index.
		for i := 0; i < 4; i++ {
			extracted, err := tree.ExtractPathForIndices(i)
			require.NoError(t, err)
			require.NotNil(t, extracted, "failed for index %d", i)
			require.Equal(t, 1, extracted.NumLeaves())
		}
	})

	t.Run("returns error for out of bounds index", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		extracted, err := tree.ExtractPathForIndices(999)
		require.Error(t, err)
		require.Nil(t, extracted)
	})

	t.Run("returns error for negative index", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		extracted, err := tree.ExtractPathForIndices(-1)
		require.Error(t, err)
		require.Nil(t, extracted)
	})

	t.Run("extracts multiple indices", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 4 leaves.
		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000 * (i + 1)),
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Extract leaves at index 1 and 3.
		extracted, err := tree.ExtractPathForIndices(1, 3)
		require.NoError(t, err)
		require.NotNil(t, extracted)
		require.Equal(t, 2, extracted.NumLeaves())
	})
}

// TestVerifyVTXOPath tests VTXO path verification.
func TestVerifyVTXOPath(t *testing.T) {
	t.Parallel()

	t.Run("verifies valid VTXO path", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		vtxoScript := []byte("vtxo_script_1234")
		leaves := []LeafDescriptor{
			{
				PkScript:    vtxoScript,
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.VerifyVTXOPath(ownerKey, vtxoScript)
		require.NoError(t, err)
	})

	t.Run("rejects wrong VTXO script", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("actual_script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.VerifyVTXOPath(ownerKey, []byte("wrong_script"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "VTXO script mismatch")
	})

	t.Run("rejects non-existent cosigner", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)
		_, unknownKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.VerifyVTXOPath(unknownKey, []byte("script"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "no path found")
	})

	t.Run("rejects nil cosigner key", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.VerifyVTXOPath(nil, []byte("script"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "cosigner key cannot be nil")
	})

	t.Run("rejects empty expected script", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.VerifyVTXOPath(ownerKey, []byte{})
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"expected VTXO script cannot be empty",
		)
	})

	t.Run("verifies cosigner in all nodes on path", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, key1 := createTestKey(t)
		_, key2 := createTestKey(t)

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

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Verify key1's path - should have key1 in all nodes.
		err = tree.VerifyVTXOPath(key1, []byte("script1"))
		require.NoError(t, err)
	})
}

// TestSubmitTreeSigs tests batch signature submission.
func TestSubmitTreeSigs(t *testing.T) {
	t.Parallel()

	t.Run("stores all signatures", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create tree with 2 leaves (3 transactions total).
		leaves := make([]LeafDescriptor, 2)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Create signatures for all transactions.
		sigs := make(map[TxID]*schnorr.Signature)
		for node := range tree.Root.NodesIter() {
			txid, _ := node.TXID()
			sigs[txid] = createTestSignature(t)
		}

		// Initially no signatures.
		for node := range tree.Root.NodesIter() {
			require.Nil(t, node.Signature)
		}

		// Submit signatures.
		err = tree.SubmitTreeSigs(sigs)
		require.NoError(t, err)

		// All nodes should have signatures.
		for node := range tree.Root.NodesIter() {
			require.NotNil(t, node.Signature)
		}
	})

	t.Run("rejects nil sigs map", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.SubmitTreeSigs(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signatures map cannot be nil")
	})

	t.Run("rejects missing signature", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		leaves := make([]LeafDescriptor, 2)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Create signatures for only some transactions.
		sigs := make(map[TxID]*schnorr.Signature)
		// Leave one transaction without signature.

		err = tree.SubmitTreeSigs(sigs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signature not found")
	})
}

// TestTreeMetrics tests tree metric helper methods.
func TestTreeMetrics(t *testing.T) {
	t.Parallel()

	t.Run("single leaf tree metrics", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		require.Equal(t, 1, tree.NumLeaves())
		require.Equal(t, 1, tree.Depth())
		require.Equal(t, 1, tree.NumTx())
	})

	t.Run("binary tree metrics", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		require.Equal(t, 4, tree.NumLeaves())
		require.Equal(t, 3, tree.Depth())
		require.Equal(t, 7, tree.NumTx())
	})

	t.Run("different radix affects depth", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 8 leaves.
		leaves := make([]LeafDescriptor, 8)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		// Binary tree (radix=2).
		tree2, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Quad-tree (radix=4).
		tree4, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			4,
		)
		require.NoError(t, err)

		// Quad-tree should have smaller depth.
		require.Less(t, tree4.Depth(), tree2.Depth())
		// Both should have same number of leaves.
		require.Equal(t, tree2.NumLeaves(), tree4.NumLeaves())
		require.Equal(t, 8, tree2.NumLeaves())
	})
}

// TestTreePrettyPrint tests Tree.PrettyPrint.
func TestTreePrettyPrint(t *testing.T) {
	t.Run("pretty prints tree", func(t *testing.T) {
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		output := tree.PrettyPrint()
		require.NotEmpty(t, output)
		require.Contains(t, output, "Transaction Tree")
	})
}

// TestTreeExtractTxids tests Tree.ExtractTxids.
func TestTreeExtractTxids(t *testing.T) {
	t.Parallel()

	t.Run("nil tree returns nil", func(t *testing.T) {
		t.Parallel()

		var nilTree *Tree
		entries, err := nilTree.ExtractTxids()
		require.NoError(t, err)
		require.Nil(t, entries)
	})

	t.Run("tree with nil root returns nil", func(t *testing.T) {
		t.Parallel()

		emptyTree := &Tree{Root: nil}
		entries, err := emptyTree.ExtractTxids()
		require.NoError(t, err)
		require.Nil(t, entries)
	})

	t.Run("single leaf tree returns one entry", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		entries, err := tree.ExtractTxids()
		require.NoError(t, err)
		require.Len(t, entries, 1)

		// Single node should be at level 0.
		require.Equal(t, 0, entries[0].TreeLevel)
	})

	t.Run("binary tree has correct levels", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		// Create 4 leaves for a 3-level binary tree.
		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		entries, err := tree.ExtractTxids()
		require.NoError(t, err)

		// Should have 7 entries (1 root + 2 branches + 4 leaves).
		require.Len(t, entries, 7)

		// Count nodes at each level.
		levelCounts := make(map[int]int)
		for _, e := range entries {
			levelCounts[e.TreeLevel]++
		}

		// Level 0: 1 root, Level 1: 2 branches, Level 2: 4 leaves.
		require.Equal(t, 1, levelCounts[0], "should have 1 root")
		require.Equal(t, 2, levelCounts[1], "should have 2 branches")
		require.Equal(t, 4, levelCounts[2], "should have 4 leaves")
	})

	t.Run("all txids are unique", func(t *testing.T) {
		t.Parallel()
		_, operatorKey := createTestKey(t)

		leaves := make([]LeafDescriptor, 4)
		for i := range leaves {
			_, key := createTestKey(t)
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: key,
			}
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		entries, err := tree.ExtractTxids()
		require.NoError(t, err)

		// Verify all txids are unique.
		seen := make(map[string]bool)
		for _, e := range entries {
			txidStr := e.Txid.String()
			require.False(
				t, seen[txidStr], "duplicate txid found: %s",
				txidStr,
			)
			seen[txidStr] = true
		}
	})
}

// TestValidatePath tests the ValidatePath method.
func TestValidatePath(t *testing.T) {
	t.Parallel()

	t.Run("nil tree returns error", func(t *testing.T) {
		t.Parallel()

		var tree *Tree
		_, err := tree.ValidatePath(nil, LeafDescriptor{}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "tree is nil")
	})

	t.Run("nil signing key returns error", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: 1000,
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		expectedLeaf := LeafDescriptor{
			PkScript:    []byte("script"),
			Amount:      1000,
			CoSignerKey: ownerKey,
		}
		_, err = tree.ValidatePath(nil, expectedLeaf, operatorKey)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cosigner key cannot be nil")
	})

	t.Run("valid path returns client tree", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)
		vtxoScript := []byte("vtxo_script")

		leaves := []LeafDescriptor{
			{
				PkScript:    vtxoScript,
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		expectedLeaf := LeafDescriptor{
			PkScript:    vtxoScript,
			Amount:      1000,
			CoSignerKey: ownerKey,
		}
		clientTree, err := tree.ValidatePath(
			ownerKey, expectedLeaf, operatorKey,
		)
		require.NoError(t, err)
		require.NotNil(t, clientTree)

		// Verify client tree has exactly one leaf.
		leafNodes := clientTree.Root.GetLeafNodes()
		require.Len(t, leafNodes, 1)
	})

	t.Run("amount mismatch returns error", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)
		vtxoScript := []byte("vtxo_script")

		leaves := []LeafDescriptor{
			{
				PkScript:    vtxoScript,
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		// Use wrong amount to trigger validation error.
		expectedLeaf := LeafDescriptor{
			PkScript:    vtxoScript,
			Amount:      2000,
			CoSignerKey: ownerKey,
		}
		_, err = tree.ValidatePath(ownerKey, expectedLeaf, operatorKey)
		require.Error(t, err)
		require.Contains(t, err.Error(), "VTXO output value")
	})

	t.Run("missing operator key returns error", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)
		_, wrongOperatorKey := createTestKey(t)
		vtxoScript := []byte("vtxo_script")

		leaves := []LeafDescriptor{
			{
				PkScript:    vtxoScript,
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{
				Hash: chainhash.HashH([]byte("batch")),
			}, &wire.TxOut{
				Value: sumLeafAmounts(leaves),
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		expectedLeaf := LeafDescriptor{
			PkScript:    vtxoScript,
			Amount:      1000,
			CoSignerKey: ownerKey,
		}

		// Use wrong operator key to trigger validation error.
		_, err = tree.ValidatePath(
			ownerKey, expectedLeaf, wrongOperatorKey,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key")
	})
}

// TestValidateAndSubmitSignatures tests the ValidateAndSubmitSignatures method.
func TestValidateAndSubmitSignatures(t *testing.T) {
	t.Parallel()

	t.Run("nil tree returns error", func(t *testing.T) {
		t.Parallel()

		fakeTxid := chainhash.HashH([]byte("fake-tx"))
		var tree *Tree
		err := tree.ValidateAndSubmitSignatures(
			map[chainhash.Hash][]byte{
				fakeTxid: {0x01},
			},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "tree is nil")
	})

	t.Run("empty signatures returns error", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: 1000,
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		emptySigs := map[chainhash.Hash][]byte{}
		err = tree.ValidateAndSubmitSignatures(emptySigs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no signatures provided")
	})

	t.Run("nil signatures returns error", func(t *testing.T) {
		t.Parallel()

		_, operatorKey := createTestKey(t)
		_, ownerKey := createTestKey(t)

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      1000,
				CoSignerKey: ownerKey,
			},
		}

		tree, err := NewTree(
			wire.OutPoint{}, &wire.TxOut{
				Value: 1000,
			},
			leaves,
			operatorKey,
			make([]byte, 32),
			2,
		)
		require.NoError(t, err)

		err = tree.ValidateAndSubmitSignatures(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no signatures provided")
	})
}

// TestValidateAnchors tests the ValidateAnchors method.
func TestValidateAnchors(t *testing.T) {
	t.Parallel()

	t.Run("nil tree returns error", func(t *testing.T) {
		t.Parallel()

		var tree *Tree
		err := tree.ValidateAnchors()
		require.Error(t, err)
		require.Contains(t, err.Error(), "tree is nil")
	})
}
