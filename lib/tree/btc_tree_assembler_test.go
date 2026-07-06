package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestBTCTreeAssemblerBuildTree tests the BTC tree assembler's two-pass
// construction flow.
func TestBTCTreeAssemblerBuildTree(t *testing.T) {
	t.Parallel()

	// Generate test keys.
	operatorKey := genPubKey(t)
	sweepKey := genPubKey(t)
	sweepTapscriptRoot := computeSweepTapscriptRoot(t, sweepKey)

	t.Run("single leaf tree", func(t *testing.T) {
		t.Parallel()

		// Create a single leaf descriptor.
		leafKey := genPubKey(t)
		leafPkScript := genTaprootPkScript(t, leafKey)

		leaves := []LeafDescriptor{{
			CoSignerKey: leafKey,
			PkScript:    leafPkScript,
			Amount:      btcutil.Amount(10000),
		}}

		// Build tree.
		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
			Radix:              4,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root-input")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(10000, rootPkScript)

		tree, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify tree structure.
		require.NotNil(t, tree.Root)
		require.True(t, tree.Root.IsLeaf())
		require.Equal(t, rootInput, tree.BatchOutpoint)
		require.Equal(t, rootOutput, tree.BatchOutput)
		require.Equal(t, sweepTapscriptRoot, tree.SweepTapscriptRoot)

		// Verify leaf has outputs set.
		require.NotNil(t, tree.Root.Outputs)
		require.Len(t, tree.Root.Outputs, 2) // leaf output + anchor

		// Verify leaf output uses the provided pkscript.
		require.Equal(t, leafPkScript, tree.Root.Outputs[0].PkScript)

		// Verify final key is set.
		require.NotNil(t, tree.Root.FinalKey)

		// Verify the tree passes verification.
		require.NoError(t, tree.Verify())
	})

	t.Run("multi-leaf tree with branching", func(t *testing.T) {
		t.Parallel()

		// Create multiple leaves to force branching.
		var leaves []LeafDescriptor
		for i := 0; i < 5; i++ {
			leafKey := genPubKey(t)
			leaves = append(leaves, LeafDescriptor{
				CoSignerKey: leafKey,
				PkScript:    genTaprootPkScript(t, leafKey),
				Amount:      btcutil.Amount(10000),
			})
		}

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
			Radix:              2, // binary tree
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("root-input-multi")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(50000, rootPkScript)

		tree, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Root should be a branch with children.
		require.False(t, tree.Root.IsLeaf())
		require.NotEmpty(t, tree.Root.Children)

		// Verify outputs are set on root.
		require.NotNil(t, tree.Root.Outputs)

		// Count leaves to verify tree structure.
		leafCount := 0
		err = tree.Root.ForEachLeaf(func(n *Node) error {
			leafCount++
			// Each leaf should have outputs and final key set.
			require.NotNil(t, n.Outputs)
			require.NotNil(t, n.FinalKey)

			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 5, leafCount)

		// Verify the tree passes verification.
		require.NoError(t, tree.Verify())
	})

	t.Run("error cases", func(t *testing.T) {
		t.Parallel()

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(10000, rootPkScript)

		t.Run("empty leaves fails", func(t *testing.T) {
			_, err := assembler.BuildTree(
				rootInput, rootOutput, nil,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), "no leaves supplied")
		})

		t.Run("nil root output fails", func(t *testing.T) {
			leafKey := genPubKey(t)
			leaves := []LeafDescriptor{{
				CoSignerKey: leafKey,
				PkScript:    genTaprootPkScript(t, leafKey),
			}}
			_, err := assembler.BuildTree(rootInput, nil, leaves)
			require.Error(t, err)
			require.Contains(
				t, err.Error(),
				"root output cannot be nil",
			)
		})
	})

	t.Run("nil operator key fails", func(t *testing.T) {
		t.Parallel()

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        nil,
			SweepTapscriptRoot: sweepTapscriptRoot,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(10000, rootPkScript)
		leafKey := genPubKey(t)
		leaves := []LeafDescriptor{{
			CoSignerKey: leafKey,
			PkScript:    genTaprootPkScript(t, leafKey),
		}}

		_, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key cannot be nil")
	})

	t.Run("nil sweep tapscript root works for connectors", func(
		t *testing.T) {

		t.Parallel()

		// Connector trees don't have sweep scripts, so nil is valid.
		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: nil,
			Radix:              4,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("test")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(10000, rootPkScript)
		leafKey := genPubKey(t)
		leaves := []LeafDescriptor{{
			CoSignerKey: leafKey,
			PkScript:    genTaprootPkScript(t, leafKey),
			Amount:      btcutil.Amount(10000),
		}}

		tree, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify the tree passes verification.
		require.NoError(t, tree.Verify())
	})

	t.Run("default radix is applied", func(t *testing.T) {
		t.Parallel()

		// Create enough leaves to see branching with default radix.
		var leaves []LeafDescriptor
		for i := 0; i < 8; i++ {
			leafKey := genPubKey(t)
			leaves = append(leaves, LeafDescriptor{
				CoSignerKey: leafKey,
				PkScript:    genTaprootPkScript(t, leafKey),
				Amount:      btcutil.Amount(1000),
			})
		}

		// Config with Radix=0 should default to 4.
		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
			Radix:              0,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("default-radix")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(8000, rootPkScript)

		tree, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// With 8 leaves and radix 4, root should have children.
		require.False(t, tree.Root.IsLeaf())

		// Verify structure is correct.
		require.NoError(t, tree.Verify())
	})

	t.Run("unequal leaf amounts produce correct outputs", func(
		t *testing.T) {

		t.Parallel()

		// Create leaves with different amounts to verify each leaf's
		// output uses the correct value from its descriptor.
		amounts := []btcutil.Amount{5000, 15000, 3000, 7000}
		var leaves []LeafDescriptor
		leafKeys := make([]*btcec.PublicKey, len(amounts))

		for i, amt := range amounts {
			leafKey := genPubKey(t)
			leafKeys[i] = leafKey
			leaves = append(leaves, LeafDescriptor{
				CoSignerKey: leafKey,
				PkScript:    genTaprootPkScript(t, leafKey),
				Amount:      amt,
			})
		}

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
			// Binary tree for predictable structure.
			Radix: 2,
		})

		totalAmount := btcutil.Amount(30000)
		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("unequal-amounts")),
			Index: 0,
		}
		rootPkScript := genTaprootPkScript(t, operatorKey)
		rootOutput := wire.NewTxOut(int64(totalAmount), rootPkScript)

		tree, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Collect all leaf output amounts.
		var leafOutputAmounts []btcutil.Amount
		err = tree.Root.ForEachLeaf(func(n *Node) error {
			require.NotNil(t, n.Outputs)
			require.GreaterOrEqual(t, len(n.Outputs), 1)

			// First output is the leaf output (not anchor).
			leafOutputAmounts = append(
				leafOutputAmounts,
				btcutil.Amount(n.Outputs[0].Value),
			)

			return nil
		})
		require.NoError(t, err)

		// Verify we have 4 leaves with outputs.
		require.Len(t, leafOutputAmounts, 4)

		// Sum of all leaf outputs should equal sum of input amounts.
		var totalLeafOutput btcutil.Amount
		for _, amt := range leafOutputAmounts {
			totalLeafOutput += amt
		}
		require.Equal(t, totalAmount, totalLeafOutput)

		// Verify tree structure is valid.
		require.NoError(t, tree.Verify())
	})

	t.Run("leaf amounts exceeding root value fails", func(t *testing.T) {
		t.Parallel()

		// Create leaves whose total exceeds the root output value.
		leafKey1 := genPubKey(t)
		leafKey2 := genPubKey(t)
		leaves := []LeafDescriptor{
			{
				CoSignerKey: genPubKey(t),
				PkScript:    genTaprootPkScript(t, leafKey1),
				Amount:      btcutil.Amount(10000),
			},
			{
				CoSignerKey: genPubKey(t),
				PkScript:    genTaprootPkScript(t, leafKey2),
				Amount:      btcutil.Amount(10000),
			},
		}

		assembler := NewTreeAssembler(TreeConfig{
			OperatorKey:        operatorKey,
			SweepTapscriptRoot: sweepTapscriptRoot,
			Radix:              4,
		})

		rootInput := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("exceeds-value")),
			Index: 0,
		}

		// Root output value (15000) is less than sum of leaf amounts
		// (20000).
		rootOutput := wire.NewTxOut(
			15000, genTaprootPkScript(t, operatorKey),
		)

		_, err := assembler.BuildTree(rootInput, rootOutput, leaves)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must equal the output value")
	})
}

// genPubKey generates a random public key for testing.
func genPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privKey.PubKey()
}

// genTaprootPkScript generates a P2TR script for the given key.
func genTaprootPkScript(t *testing.T, key *btcec.PublicKey) []byte {
	t.Helper()

	pkScript, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return pkScript
}

// computeSweepTapscriptRoot computes a tapscript root for a sweep key.
func computeSweepTapscriptRoot(t *testing.T, sweepKey *btcec.PublicKey) []byte {
	t.Helper()

	// Create a simple tapscript tree with a single leaf.
	sweepScript, err := txscript.NewScriptBuilder().
		AddData(sweepKey.SerializeCompressed()).
		AddOp(txscript.OP_CHECKSIG).
		Script()
	require.NoError(t, err)

	tapLeaf := txscript.NewBaseTapLeaf(sweepScript)
	tree := txscript.AssembleTaprootScriptTree(tapLeaf)
	root := tree.RootNode.TapHash()

	return root[:]
}
