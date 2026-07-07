package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestPartitionLeaves tests the partitionLeaves function.
func TestPartitionLeaves(t *testing.T) {
	t.Parallel()

	// Create test leaf descriptors.
	createLeaves := func(count int) []LeafDescriptor {
		leaves := make([]LeafDescriptor, count)
		for i := 0; i < count; i++ {
			privKey, _ := btcec.NewPrivateKey()
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: privKey.PubKey(),
			}
		}

		return leaves
	}

	t.Run("fewer leaves than radix", func(t *testing.T) {
		t.Parallel()

		// 3 leaves with radix 5 should create 3 groups of 1 leaf each.
		leaves := createLeaves(3)
		groups := partitionLeaves(leaves, 5, nil)

		require.Len(t, groups, 3)
		for i, group := range groups {
			require.Len(
				t, group, 1, "group %d should have 1 leaf", i,
			)
		}
	})

	t.Run("equal leaves and radix", func(t *testing.T) {
		t.Parallel()

		// 4 leaves with radix 4 should create 4 groups of 1 leaf each.
		leaves := createLeaves(4)
		groups := partitionLeaves(leaves, 4, nil)

		require.Len(t, groups, 4)
		for i, group := range groups {
			require.Len(
				t, group, 1, "group %d should have 1 leaf", i,
			)
		}
	})

	t.Run("more leaves than radix distributes evenly", func(t *testing.T) {
		t.Parallel()

		// 8 leaves with radix 4 should create 4 groups of 2 leaves
		// each.
		leaves := createLeaves(8)
		groups := partitionLeaves(leaves, 4, nil)

		require.Len(t, groups, 4)
		for i, group := range groups {
			require.Len(
				t, group, 2, "group %d should have 2 leaves", i,
			)
		}
	})

	t.Run("uneven distribution", func(t *testing.T) {
		t.Parallel()

		// 10 leaves with radix 4 should create:
		// - 2 groups with 3 leaves (10 % 4 = 2 extra).
		// - 2 groups with 2 leaves (10 / 4 = 2 base).
		leaves := createLeaves(10)
		groups := partitionLeaves(leaves, 4, nil)

		require.Len(t, groups, 4)

		// Count leaves per group.
		counts := make([]int, len(groups))
		for i, group := range groups {
			counts[i] = len(group)
		}

		// Verify distribution: first 2 groups should have 3 leaves,
		// last 2 should have 2 leaves.
		require.Equal(t, 3, counts[0])
		require.Equal(t, 3, counts[1])
		require.Equal(t, 2, counts[2])
		require.Equal(t, 2, counts[3])

		// Verify total leaf count.
		totalLeaves := 0
		for _, group := range groups {
			totalLeaves += len(group)
		}
		require.Equal(t, 10, totalLeaves)
	})

	t.Run("single leaf", func(t *testing.T) {
		t.Parallel()

		leaves := createLeaves(1)
		groups := partitionLeaves(leaves, 4, nil)

		require.Len(t, groups, 1)
		require.Len(t, groups[0], 1)
	})

	t.Run("two leaves with radix 4", func(t *testing.T) {
		t.Parallel()

		leaves := createLeaves(2)
		groups := partitionLeaves(leaves, 4, nil)

		// Should create 2 groups with 1 leaf each.
		require.Len(t, groups, 2)
		require.Len(t, groups[0], 1)
		require.Len(t, groups[1], 1)
	})

	t.Run("fallback for degenerate case", func(t *testing.T) {
		t.Parallel()

		// This test verifies the safety fallback that ensures at least
		// 2 non-empty groups when M > 1. While the normal round-robin
		// algorithm should prevent this, the fallback provides defense
		// in depth.
		leaves := createLeaves(2)
		groups := partitionLeaves(leaves, 100, nil)

		// Should have exactly 2 groups (fallback split in half).
		require.Len(t, groups, 2)
		require.Len(t, groups[0], 1)
		require.Len(t, groups[1], 1)
	})

	t.Run("radix 2 binary tree", func(t *testing.T) {
		t.Parallel()

		// 7 leaves with radix 2 should create 2 groups:
		// - Group 0: 4 leaves (7 / 2 = 3 base + 1 extra).
		// - Group 1: 3 leaves (7 / 2 = 3 base).
		leaves := createLeaves(7)
		groups := partitionLeaves(leaves, 2, nil)

		require.Len(t, groups, 2)
		require.Len(t, groups[0], 4)
		require.Len(t, groups[1], 3)
	})

	t.Run("large radix", func(t *testing.T) {
		t.Parallel()

		// 100 leaves with radix 10.
		leaves := createLeaves(100)
		groups := partitionLeaves(leaves, 10, nil)

		require.Len(t, groups, 10)

		// Each group should have exactly 10 leaves.
		for i, group := range groups {
			require.Len(
				t, group, 10, "group %d should have 10 leaves",
				i,
			)
		}
	})

	t.Run("verifies all leaves are assigned", func(t *testing.T) {
		t.Parallel()

		leaves := createLeaves(17)
		groups := partitionLeaves(leaves, 5, nil)

		// Count total leaves across all groups.
		totalLeaves := 0
		for _, group := range groups {
			totalLeaves += len(group)
		}

		require.Equal(
			t, 17, totalLeaves,
			"all leaves should be assigned to groups",
		)
	})

	t.Run("weighted partition prefers heavy leaf alone", func(t *testing.T) { //nolint:ll
		t.Parallel()

		// Test that weighted partitioning groups leaves by BTC amount.
		// The heavy leaf (10000 sats) should go to its own group while
		// the two light leaves (1000 sats each) share the other group,
		// balancing total weight across groups.
		leaves := []LeafDescriptor{
			{
				PkScript: []byte("heavy"),
				Amount:   10_000,
			},
			{
				PkScript: []byte("light1"),
				Amount:   1000,
			},
			{
				PkScript: []byte("light2"),
				Amount:   1000,
			},
		}

		weightFn := WeightByBtcAmount()

		groups := partitionLeaves(leaves, 2, weightFn)

		require.Len(t, groups, 2)
		require.Len(t, groups[0], 1)
		require.Len(t, groups[1], 2)
	})
}

// TestBuildTreeBFS tests the BTCTreeAssembler.BuildTree function.
func TestBuildTreeBFS(t *testing.T) {
	t.Parallel()

	// Helper to create test leaf descriptors.
	createLeaves := func(count int) ([]LeafDescriptor, []*btcec.PublicKey) {
		leaves := make([]LeafDescriptor, count)
		keys := make([]*btcec.PublicKey, count)
		for i := 0; i < count; i++ {
			privKey, _ := btcec.NewPrivateKey()
			keys[i] = privKey.PubKey()
			leaves[i] = LeafDescriptor{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: keys[i],
			}
		}

		return leaves, keys
	}

	// Helper to create a test input.
	createTestInput := func() wire.OutPoint {
		return wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root")),
			Index: 0,
		}
	}

	// Helper to create a test operator key.
	createOperatorKey := func() *btcec.PublicKey {
		privKey, _ := btcec.NewPrivateKey()

		return privKey.PubKey()
	}

	// runBuild is a helper to build a tree using BTCTreeAssembler.
	runBuild := func(t *testing.T, input wire.OutPoint,
		leaves []LeafDescriptor, operatorKey *btcec.PublicKey,
		sweepRoot []byte, radix int, weight PartitionWeightFunc) (*Node,
		error) {

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepRoot,
			Radix:              radix,
			WeightFn:           weight,
		})

		rootOutput := &wire.TxOut{
			Value:    1000 * int64(len(leaves)),
			PkScript: []byte("root"),
		}

		tree, err := assembler.BuildTree(input, rootOutput, leaves)
		if err != nil {
			return nil, err
		}

		return tree.Root, nil
	}

	t.Run("single leaf creates leaf node", func(t *testing.T) {
		t.Parallel()

		leaves, _ := createLeaves(1)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 4, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify it's a leaf node.
		require.True(t, root.IsLeaf())
		require.Equal(t, input, root.Input)

		// Verify outputs: leaf output + anchor.
		require.Len(t, root.Outputs, 2)
		require.Equal(t, int64(1000), root.Outputs[0].Value)
		require.Equal(t, int64(0), root.Outputs[1].Value)

		// Verify cosigners: owner + operator.
		require.Len(t, root.CoSigners, 2)
	})

	t.Run("two leaves creates branch with two leaf children",
		func(t *testing.T) {
			t.Parallel()

			leaves, _ := createLeaves(2)
			operatorKey := createOperatorKey()
			input := createTestInput()
			sweepRoot := make([]byte, 32)

			root, err := runBuild(
				t, input, leaves, operatorKey, sweepRoot, 4,
				nil,
			)
			require.NoError(t, err)
			require.NotNil(t, root)

			// Root should be a branch.
			require.False(t, root.IsLeaf())
			require.Len(t, root.Children, 2)

			// Verify children are leaves.
			child0 := root.Children[0]
			child1 := root.Children[1]
			require.NotNil(t, child0)
			require.NotNil(t, child1)
			require.True(t, child0.IsLeaf())
			require.True(t, child1.IsLeaf())

			// Verify tree structure integrity.
			err = root.Verify()
			require.NoError(t, err)
		})

	t.Run("four leaves with radix 2 creates balanced tree",
		func(t *testing.T) {
			t.Parallel()

			leaves, _ := createLeaves(4)
			operatorKey := createOperatorKey()
			input := createTestInput()
			sweepRoot := make([]byte, 32)

			root, err := runBuild(
				t, input, leaves, operatorKey, sweepRoot, 2,
				nil,
			)
			require.NoError(t, err)
			require.NotNil(t, root)

			// Root should be a branch with 2 children.
			require.False(t, root.IsLeaf())
			require.Len(t, root.Children, 2)

			// Each child should also be a branch with 2 leaf
			// children.
			for i := uint32(0); i < 2; i++ {
				branch := root.Children[i]
				require.NotNil(
					t, branch, "child %d should exist", i,
				)
				require.Len(
					t, branch.Children, 2, "branch %d "+
						"should have 2 children", i,
				)

				// Verify grandchildren are leaves.
				for j := uint32(0); j < 2; j++ {
					leaf := branch.Children[j]
					require.NotNil(
						t, leaf, "leaf [%d][%d] "+
							"should exist", i, j,
					)
					require.True(
						t, leaf.IsLeaf(),
						"node [%d][%d] should be leaf",
						i, j,
					)
				}
			}

			// Verify tree structure.
			err = root.Verify()
			require.NoError(t, err)

			// Verify depth and transaction count.
			require.Equal(t, 3, root.Depth())
			require.Equal(t, 7, root.NumTx())
		})

	t.Run("eight leaves with radix 4 creates proper tree",
		func(t *testing.T) {
			t.Parallel()

			leaves, _ := createLeaves(8)
			operatorKey := createOperatorKey()
			input := createTestInput()
			sweepRoot := make([]byte, 32)

			root, err := runBuild(
				t, input, leaves, operatorKey, sweepRoot, 4,
				nil,
			)
			require.NoError(t, err)
			require.NotNil(t, root)

			// Root should be a branch with 4 children.
			require.False(t, root.IsLeaf())
			require.Len(t, root.Children, 4)

			// Each child should be a branch with 2 leaf children.
			for i := uint32(0); i < 4; i++ {
				branch := root.Children[i]
				require.NotNil(
					t, branch, "child %d should exist", i,
				)
				require.Len(
					t, branch.Children, 2, "branch %d "+
						"should have 2 children", i,
				)

				// Verify grandchildren are leaves.
				for j := uint32(0); j < 2; j++ {
					leaf := branch.Children[j]
					require.NotNil(
						t, leaf, "leaf [%d][%d] "+
							"should exist", i, j,
					)
					require.True(
						t, leaf.IsLeaf(),
						"node [%d][%d] should be leaf",
						i, j,
					)
				}
			}

			// Verify tree structure.
			err = root.Verify()
			require.NoError(t, err)

			// Verify depth: root -> 4 branches -> 8 leaves = 3
			// levels.
			require.Equal(t, 3, root.Depth())

			// Verify transaction count: 1 root + 4 branches + 8
			// leaves = 13.
			require.Equal(t, 13, root.NumTx())
		})

	t.Run("verifies all leaves are reachable", func(t *testing.T) {
		t.Parallel()

		leaves, keys := createLeaves(10)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 4, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Count leaves in the tree.
		leafCount := 0
		for range root.LeavesIter() {
			leafCount++
		}
		require.Equal(t, 10, leafCount)

		// Verify each original leaf owner key has a corresponding tree
		// leaf.
		for i, key := range keys {
			leaf := root.GetLeafForCoSigner(key)
			require.NotNil(
				t, leaf, "leaf for key %d should exist", i,
			)
		}
	})

	t.Run("tree structure is valid", func(t *testing.T) {
		t.Parallel()

		leaves, _ := createLeaves(7)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 3, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify tree structure using the Verify method.
		err = root.Verify()
		require.NoError(t, err)
	})

	t.Run("radix 2 creates binary tree", func(t *testing.T) {
		t.Parallel()

		leaves, _ := createLeaves(3)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 2, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Root should have at most 2 children (binary tree).
		require.LessOrEqual(t, len(root.Children), 2)

		// Verify all nodes have at most 2 children.
		for node := range root.NodesIter() {
			require.LessOrEqual(
				t, len(node.Children), 2,
				"node should have at most 2 children",
			)
		}
	})

	t.Run("large tree builds correctly", func(t *testing.T) {
		t.Parallel()

		// 100 leaves with radix 10.
		leaves, _ := createLeaves(100)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 10, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify all 100 leaves are in the tree.
		leafCount := 0
		for range root.LeavesIter() {
			leafCount++
		}
		require.Equal(t, 100, leafCount)

		// Verify tree structure.
		err = root.Verify()
		require.NoError(t, err)
	})

	t.Run("operator key is included in all nodes", func(t *testing.T) {
		t.Parallel()

		leaves, _ := createLeaves(5)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 4, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify operator key is in all nodes.
		for node := range root.NodesIter() {
			require.True(
				t,
				ContainsCosigner(node.CoSigners, operatorKey),
				"operator key should be in all nodes",
			)
		}
	})

	t.Run("BFS order processes levels correctly", func(t *testing.T) {
		t.Parallel()

		// This test verifies that BFS processes nodes level by level.
		// We can verify this by checking the tree structure: all nodes
		// at the same depth should be processed before any nodes at
		// the next depth.
		leaves, _ := createLeaves(4)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 2, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify the tree has proper depth structure:
		// - Level 0: root (1 node).
		// - Level 1: 2 branches.
		// - Level 2: 4 leaves.
		// Count nodes at each level.
		level1Count := len(root.Children)
		require.Equal(t, 2, level1Count)

		level2Count := 0
		for _, child := range root.Children {
			level2Count += len(child.Children)
		}
		require.Equal(t, 4, level2Count)
	})

	t.Run("empty queue handling", func(t *testing.T) {
		t.Parallel()

		// This test verifies that the function properly handles the
		// queue. While we can't directly test the "unexpected empty
		// queue" error path (as it should never occur with correct
		// logic), we can verify that the queue is properly exhausted.
		leaves, _ := createLeaves(2)
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		root, err := runBuild(
			t, input, leaves, operatorKey, sweepRoot, 4, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, root)

		// Verify the tree was fully built (all leaves reachable).
		leafCount := 0
		for range root.LeavesIter() {
			leafCount++
		}
		require.Equal(t, 2, leafCount)
	})
}

// TestBuildTreeBFSEdgeCases tests edge cases and error conditions.
func TestBuildTreeBFSEdgeCases(t *testing.T) {
	t.Parallel()

	// Helper to create a test operator key.
	createOperatorKey := func() *btcec.PublicKey {
		privKey, _ := btcec.NewPrivateKey()

		return privKey.PubKey()
	}

	// Helper to create test input.
	createTestInput := func() wire.OutPoint {
		return wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root")),
			Index: 0,
		}
	}

	// runBuild is a helper to build a tree using BTCTreeAssembler.
	runBuild := func(t *testing.T, input wire.OutPoint,
		leaves []LeafDescriptor, operatorKey *btcec.PublicKey,
		sweepRoot []byte, radix int) (*Node, error) {

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepRoot,
			Radix:              radix,
			// Use the default weight function.
			WeightFn: nil,
		})

		// Create a mock root output for the assembler.
		rootOutput := &wire.TxOut{
			Value:    1000 * int64(len(leaves)),
			PkScript: []byte("root"),
		}

		tree, err := assembler.BuildTree(input, rootOutput, leaves)
		if err != nil {
			return nil, err
		}

		return tree.Root, nil
	}

	t.Run("nil operator key fails in leaf creation", func(t *testing.T) {
		t.Parallel()

		privKey, _ := btcec.NewPrivateKey()
		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: privKey.PubKey(),
			},
		}
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		// NewLeafNode doesn't validate nil operator key, but
		// NewBranchNode does. Since single leaf goes through
		// NewLeafNode, we test with 2 leaves to trigger branch
		// creation.
		leaves = append(leaves, leaves[0])

		_, err := runBuild(t, input, leaves, nil, sweepRoot, 4)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key cannot be nil")
	})

	t.Run("nil leaf cosigner key returns error", func(t *testing.T) {
		t.Parallel()

		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: nil, // Nil cosigner!
			},
			{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: nil,
			},
		}
		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		// Nil cosigner keys should cause an error during tree building.
		_, err := runBuild(t, input, leaves, operatorKey, sweepRoot, 4)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cosigner key cannot be nil")
	})

	t.Run("various radix values work correctly", func(t *testing.T) {
		t.Parallel()

		privKey, _ := btcec.NewPrivateKey()
		leaves := []LeafDescriptor{
			{
				PkScript:    []byte("script"),
				Amount:      btcutil.Amount(1000),
				CoSignerKey: privKey.PubKey(),
			},
		}

		// Duplicate leaves to get 10 total.
		for i := 0; i < 9; i++ {
			leaves = append(leaves, leaves[0])
		}

		operatorKey := createOperatorKey()
		input := createTestInput()
		sweepRoot := make([]byte, 32)

		// Test with different radix values.
		for _, radix := range []int{2, 3, 4, 5, 10} {
			root, err := runBuild(
				t, input, leaves, operatorKey, sweepRoot, radix,
			)
			require.NoError(t, err)
			require.NotNil(t, root)

			// Verify tree structure.
			err = root.Verify()
			require.NoError(t, err)

			// Verify all 10 leaves are present.
			leafCount := 0
			for range root.LeavesIter() {
				leafCount++
			}
			require.Equal(t, 10, leafCount)
		}
	})
}
