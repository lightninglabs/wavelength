package batch

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	// testClientKeyStartIndex keeps synthetic client keys disjoint from
	// the operator and sweep keys used by these tests.
	testClientKeyStartIndex = 100
)

// makeVTXODescriptors creates n VTXO descriptors with unique client keys. Each
// descriptor has an amount of baseAmount * (i+1) where i is the index.
func makeVTXODescriptors(t *testing.T, n int, baseAmount btcutil.Amount,
	operatorPub *btcec.PublicKey) []tree.VTXODescriptor {

	t.Helper()

	descs, _ := makeVTXODescriptorsWithKeys(t, n, baseAmount, operatorPub)

	return descs
}

// makeVTXODescriptorsWithKeys creates n VTXO descriptors with unique client
// keys, returning both the descriptors and the client keys. This is useful for
// tests that need to reference the client keys after creating the descriptors.
func makeVTXODescriptorsWithKeys(t *testing.T, n int, baseAmount btcutil.Amount,
	opKey *btcec.PublicKey) ([]tree.VTXODescriptor, []*btcec.PublicKey) {

	t.Helper()

	descriptors := make([]tree.VTXODescriptor, 0, n)
	clientKeys := make([]*btcec.PublicKey, 0, n)

	for i := 0; i < n; i++ {
		clientKey, _ := testutils.CreateKey(
			testClientKeyStartIndex + int32(i),
		)
		desc, err := tree.NewVTXODescriptor(
			baseAmount*btcutil.Amount(i+1), clientKey, opKey, 144,
		)
		require.NoError(t, err)

		descriptors = append(descriptors, *desc)
		clientKeys = append(clientKeys, clientKey)
	}

	return descriptors, clientKeys
}

// TestVTXOBatches tests that VTXOBatches correctly splits VTXOs into batches
// of the specified maximum size.
func TestVTXOBatches(t *testing.T) {
	t.Parallel()

	// makeVTXOs creates a slice of n dummy VTXODescriptors.
	makeVTXOs := func(n int) []tree.VTXODescriptor {
		vtxos := make([]tree.VTXODescriptor, n)
		for i := range vtxos {
			vtxos[i] = tree.VTXODescriptor{
				Amount: btcutil.Amount(i + 1),
			}
		}

		return vtxos
	}

	tests := []struct {
		name        string
		numVTXOs    int
		maxPerBatch uint32
		wantBatches []int
	}{
		{
			name:        "empty slice",
			numVTXOs:    0,
			maxPerBatch: 5,
			wantBatches: nil,
		},
		{
			name:        "smaller than batch size",
			numVTXOs:    3,
			maxPerBatch: 5,
			wantBatches: []int{
				3,
			},
		},
		{
			name:        "exact batch size",
			numVTXOs:    5,
			maxPerBatch: 5,
			wantBatches: []int{
				5,
			},
		},
		{
			name:        "multiple full batches",
			numVTXOs:    10,
			maxPerBatch: 5,
			wantBatches: []int{
				5,
				5,
			},
		},
		{
			name:        "multiple batches with remainder",
			numVTXOs:    12,
			maxPerBatch: 5,
			wantBatches: []int{
				5,
				5,
				2,
			},
		},
		{
			name:        "single element batches",
			numVTXOs:    3,
			maxPerBatch: 1,
			wantBatches: []int{
				1,
				1,
				1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			vtxos := makeVTXOs(tc.numVTXOs)

			// Collect batch sizes and indices.
			var (
				gotBatches []int
				gotIndices []int
			)

			batches, err := VTXOBatches(vtxos, tc.maxPerBatch)
			require.NoError(t, err)

			for idx, batch := range batches {
				gotIndices = append(gotIndices, idx)
				gotBatches = append(gotBatches, len(batch))
			}

			// Verify batch sizes match expectations.
			require.Equal(t, tc.wantBatches, gotBatches)

			// Verify indices are sequential starting from 0.
			for i, idx := range gotIndices {
				require.Equal(t, i, idx)
			}
		})
	}
}

// TestBuildTreeContext tests the BuildTreeContext function.
func TestBuildTreeContext(t *testing.T) {
	t.Parallel()

	operatorPub, _ := testutils.CreateKey(1)
	sweepPub, _ := testutils.CreateKey(2)

	terms := &Terms{
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorPub,
		},
		SweepKey: keychain.KeyDescriptor{
			PubKey: sweepPub,
		},
		SweepDelay:      288,
		MaxVTXOsPerTree: 4,
	}

	t.Run("single VTXO creates one output", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 1, 10000, operatorPub)

		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)

		outputs := ctx.Outputs()
		require.Len(t, outputs, 1)
		require.NotNil(t, outputs[0])
		require.Greater(t, outputs[0].Value, int64(0))
	})

	t.Run("multiple VTXOs under limit creates one output",
		func(t *testing.T) {
			// Create 3 VTXOs (under MaxVTXOsPerTree of 4).
			descs := makeVTXODescriptors(t, 3, 10000, operatorPub)

			ctx, err := BuildTreeContext(terms, descs)
			require.NoError(t, err)
			require.Len(t, ctx.Outputs(), 1)
		},
	)

	t.Run("VTXOs over limit split into multiple outputs",
		func(t *testing.T) {
			// Create 10 VTXOs (splits into 3 batches: 4, 4, 2).
			descs := makeVTXODescriptors(t, 10, 5000, operatorPub)

			ctx, err := BuildTreeContext(terms, descs)
			require.NoError(t, err)

			outputs := ctx.Outputs()
			require.Len(t, outputs, 3)

			// Each output should have non-zero value.
			for _, output := range outputs {
				require.Greater(t, output.Value, int64(0))
			}
		},
	)
}

// TestTreeContextBuildVTXOTreesForCommitmentTx tests building VTXO trees from
// a commitment transaction using explicit output indices.
func TestTreeContextBuildVTXOTreesForCommitmentTx(t *testing.T) {
	t.Parallel()

	operatorPub, _ := testutils.CreateKey(1)
	sweepPub, _ := testutils.CreateKey(2)

	terms := &Terms{
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorPub,
		},
		SweepKey: keychain.KeyDescriptor{
			PubKey: sweepPub,
		},
		SweepDelay:      288,
		TreeRadix:       4,
		MaxVTXOsPerTree: 4,
	}

	t.Run("empty descriptors returns empty map", func(t *testing.T) {
		ctx, err := BuildTreeContext(terms, nil)
		require.NoError(t, err)

		tx := wire.NewMsgTx(2)
		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, nil)
		require.NoError(t, err)
		require.NotNil(t, trees)
		require.Len(t, trees, 0)
	})

	t.Run("builds tree for single batch", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 2, 10000, operatorPub)

		// Build batch context first.
		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)
		require.Len(t, ctx.Outputs(), 1)

		// Create commitment tx with the batch output at index 0.
		tx := wire.NewMsgTx(2)
		tx.AddTxOut(ctx.Outputs()[0])

		// Build trees with explicit index.
		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0})
		require.NoError(t, err)
		require.Len(t, trees, 1)
		require.Contains(t, trees, 0)
		require.NotNil(t, trees[0])

		// Verify tree structure.
		vtxoTree := trees[0]
		require.NotNil(t, vtxoTree.Root)
		require.Equal(t, 2, vtxoTree.NumLeaves())
	})

	t.Run("builds multiple trees for large batch", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 10, 5000, operatorPub)

		// Build batch context.
		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)
		require.Len(t, ctx.Outputs(), 3)

		// Create commitment tx with all batch outputs.
		tx := wire.NewMsgTx(2)
		for _, output := range ctx.Outputs() {
			tx.AddTxOut(output)
		}

		// Build trees with explicit indices (0, 1, 2).
		trees, err := ctx.BuildVTXOTreesForCommitmentTx(
			tx, []int{0, 1, 2},
		)
		require.NoError(t, err)
		require.Len(t, trees, 3)

		// Verify all trees exist at correct indices.
		require.Contains(t, trees, 0)
		require.Contains(t, trees, 1)
		require.Contains(t, trees, 2)

		// Verify tree leaf counts.
		require.Equal(t, 4, trees[0].NumLeaves())
		require.Equal(t, 4, trees[1].NumLeaves())
		require.Equal(t, 2, trees[2].NumLeaves())
	})

	t.Run("batch outputs start at offset index", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 2, 10000, operatorPub)

		// Build batch context.
		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)

		// Create commitment tx with a leave output first, then batch.
		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{Value: 50000, PkScript: []byte{0}})
		tx.AddTxOut(ctx.Outputs()[0])

		// Build trees with explicit index 1 (after leave output).
		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, []int{1})
		require.NoError(t, err)
		require.Len(t, trees, 1)
		require.Contains(t, trees, 1)
		require.NotContains(t, trees, 0)
	})

	t.Run("mismatched indices count returns error", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 2, 10000, operatorPub)

		// Build batch context (produces 1 output).
		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)
		require.Len(t, ctx.Outputs(), 1)

		tx := wire.NewMsgTx(2)
		tx.AddTxOut(ctx.Outputs()[0])
		tx.AddTxOut(&wire.TxOut{Value: 50000, PkScript: []byte{0}})

		// Provide 2 indices when only 1 output exists.
		_, err = ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0, 1})
		require.ErrorContains(t, err, "does not match")
	})

	t.Run("wrong output at index returns error", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 2, 10000, operatorPub)

		// Build batch context (produces 1 output).
		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)

		// Create tx with a different output at index 0.
		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{
			Value:    12345,
			PkScript: []byte{0xde, 0xad},
		})

		// Index points to wrong output.
		_, err = ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0})
		require.ErrorContains(t, err, "does not match expected")
	})
}

// TestExtractClientVTXOPaths tests extracting client-specific paths from VTXO
// trees.
func TestExtractClientVTXOPaths(t *testing.T) {
	t.Parallel()

	operatorPub, _ := testutils.CreateKey(1)
	sweepPub, _ := testutils.CreateKey(2)

	terms := &Terms{
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorPub,
		},
		SweepKey: keychain.KeyDescriptor{
			PubKey: sweepPub,
		},
		SweepDelay:      288,
		TreeRadix:       4,
		MaxVTXOsPerTree: 4,
	}

	t.Run("empty trees returns empty", func(t *testing.T) {
		paths, err := ExtractClientVTXOPaths(
			make(map[int]*tree.Tree), nil,
		)
		require.NoError(t, err)
		require.Empty(t, paths)
	})

	t.Run("extracts path for client with single VTXO", func(t *testing.T) {
		descs, clientKeys := makeVTXODescriptorsWithKeys(
			t, 1, 10000, operatorPub,
		)

		// Build batch context and tree.
		ctx, err := BuildTreeContext(terms, descs)
		require.NoError(t, err)

		tx := wire.NewMsgTx(2)
		tx.AddTxOut(ctx.Outputs()[0])

		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0})
		require.NoError(t, err)

		// Extract client paths.
		paths, err := ExtractClientVTXOPaths(trees, clientKeys)
		require.NoError(t, err)
		require.Len(t, paths, 1)
		require.Contains(t, paths, 0)
		require.NotNil(t, paths[0])
	})

	t.Run("extracts paths for client with multiple VTXOs",
		func(t *testing.T) {
			// Create 2 VTXOs (both will be in same tree).
			descs, clientKeys := makeVTXODescriptorsWithKeys(
				t, 2, 10000, operatorPub,
			)

			// Build batch context and tree.
			ctx, err := BuildTreeContext(terms, descs)
			require.NoError(t, err)

			tx := wire.NewMsgTx(2)
			tx.AddTxOut(ctx.Outputs()[0])

			trees, err := ctx.BuildVTXOTreesForCommitmentTx(
				tx, []int{0},
			)
			require.NoError(t, err)

			// Extract client paths.
			paths, err := ExtractClientVTXOPaths(trees, clientKeys)
			require.NoError(t, err)
			require.Len(t, paths, 1)
			require.Contains(t, paths, 0)

			// Verify the path contains both client VTXOs.
			require.Equal(t, 2, paths[0].NumLeaves())
		},
	)

	t.Run("extracts paths across multiple trees", func(t *testing.T) {
		// Create 6 VTXOs (will split into 2 trees: 4 + 2).
		descs, clientKeys := makeVTXODescriptorsWithKeys(
			t, 6, 5000, operatorPub,
		)

		// Build batch context and trees.
		ctx, err := BuildTreeContext(terms, descs)
		require.NoError(t, err)
		require.Len(t, ctx.Outputs(), 2)

		tx := wire.NewMsgTx(2)
		for _, output := range ctx.Outputs() {
			tx.AddTxOut(output)
		}

		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0, 1})
		require.NoError(t, err)
		require.Len(t, trees, 2)

		// Extract client paths.
		paths, err := ExtractClientVTXOPaths(trees, clientKeys)
		require.NoError(t, err)
		require.Len(t, paths, 2)
		require.Contains(t, paths, 0)
		require.Contains(t, paths, 1)

		// Verify leaf counts (4 in first tree, 2 in second).
		require.Equal(t, 4, paths[0].NumLeaves())
		require.Equal(t, 2, paths[1].NumLeaves())
	})

	t.Run("returns nil path when client not in tree", func(t *testing.T) {
		// Create a tree with VTXOs from other clients.
		descs := makeVTXODescriptors(t, 1, 10000, operatorPub)

		// Build tree.
		ctx, err := BuildTreeContext(terms, descs)
		require.NoError(t, err)

		tx := wire.NewMsgTx(2)
		tx.AddTxOut(ctx.Outputs()[0])

		trees, err := ctx.BuildVTXOTreesForCommitmentTx(tx, []int{0})
		require.NoError(t, err)

		// Client with different key not in the tree.
		differentClientKey, _ := testutils.CreateKey(99)
		clientKeys := []*btcec.PublicKey{differentClientKey}

		// Extract paths - should be empty since client not in tree.
		paths, err := ExtractClientVTXOPaths(trees, clientKeys)
		require.NoError(t, err)
		require.Empty(t, paths)
	})
}
