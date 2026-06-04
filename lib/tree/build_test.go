package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

	// The round-robin partition (nil weightFn) is fully determined by the
	// leaf count and radix, so collapse the count/radix cases into a
	// data-only table whose expected per-group lengths also pin down the
	// total leaf count.
	rr := func(n int) []int {
		l := make([]int, n)
		for i := range l {
			l[i] = 1
		}

		return l
	}
	roundRobinCases := []struct {
		name      string
		count     int
		radix     int
		wantSizes []int
	}{
		// Fewer leaves than radix yields one leaf per group.
		{
			"fewer leaves than radix",
			3,
			5,
			rr(3),
		},

		// Equal leaves and radix yields one leaf per group.
		{
			"equal leaves and radix",
			4,
			4,
			rr(4),
		},

		// More leaves than radix distributes evenly.
		{
			"more leaves than radix",
			8,
			4,
			[]int{
				2,
				2,
				2,
				2,
			},
		},

		// Uneven: first 10%4=2 groups get the extra leaf.
		{
			"uneven distribution",
			10,
			4,
			[]int{
				3,
				3,
				2,
				2,
			},
		},

		// Single leaf yields a single group.
		{
			"single leaf",
			1,
			4,
			rr(1),
		},

		// Two leaves with radix 4 yields two singleton groups.
		{
			"two leaves with radix 4",
			2,
			4,
			rr(2),
		},

		// Degenerate radix triggers the half-split fallback that
		// guarantees at least 2 non-empty groups when M > 1.
		{
			"fallback for degenerate case",
			2,
			100,
			rr(2),
		},

		// Radix 2 binary tree: group 0 gets the 7%2=1 extra leaf.
		{
			"radix 2 binary tree",
			7,
			2,
			[]int{
				4,
				3,
			},
		},

		// Large radix evenly fills every group with 10 leaves.
		{"large radix", 100, 10, []int{
			10, 10, 10, 10, 10, 10, 10, 10, 10, 10,
		}},

		// All leaves assigned: 17%5=2 groups get the extra leaf.
		{"verifies all leaves are assigned", 17, 5,
			[]int{
				4,
				4,
				3,
				3,
				3,
			}},
	}
	for _, tc := range roundRobinCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			leaves := createLeaves(tc.count)
			groups := partitionLeaves(leaves, tc.radix, nil)

			require.Len(t, groups, len(tc.wantSizes))
			for i, group := range groups {
				require.Len(
					t, group, tc.wantSizes[i], "group "+
						"%d size", i,
				)
			}
		})
	}

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
