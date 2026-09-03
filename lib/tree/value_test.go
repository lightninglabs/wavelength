package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// valueTestTree builds a minimal tree whose root spends batchValue and creates
// the supplied output values. Scripts and anchors are irrelevant to value
// conservation, so the fixture keeps them intentionally small.
func valueTestTree(batchValue int64, outputValues ...int64) *Tree {
	batchOutpoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("value-test-batch")),
	}
	outputs := make([]*wire.TxOut, len(outputValues))
	for i, value := range outputValues {
		outputs[i] = wire.NewTxOut(value, []byte{0x51})
	}

	return &Tree{
		Root: &Node{
			Input:    batchOutpoint,
			Outputs:  outputs,
			Children: make(map[uint32]*Node),
		},
		BatchOutpoint: batchOutpoint,
		BatchOutput: wire.NewTxOut(
			batchValue, []byte{0x51},
		),
	}
}

// TestValidateValueConservation exercises the tree's zero-fee value rule and
// funding relationships at the root and at interior edges.
func TestValidateValueConservation(t *testing.T) {
	t.Parallel()

	t.Run("valid zero-fee root", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 600, 400)
		require.NoError(t, tree.ValidateValueConservation())
	})

	t.Run("non-zero fee", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 999)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"output value 999 does not equal input value 1000",
		)
	})

	t.Run("root input mismatch", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		tree.Root.Input.Index++
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "funding outpoint")
	})

	t.Run("root outputs exceed batch output", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000, 1)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"output value 1001 exceeds input",
		)

		err = tree.Verify()
		require.Error(t, err)
		require.Contains(t, err.Error(), "value conservation failed")
	})

	t.Run("interior outputs exceed parent output", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 400, 600)
		child := &Node{
			Outputs: []*wire.TxOut{
				wire.NewTxOut(401, []byte{0x51}),
			},
			Children: make(map[uint32]*Node),
		}
		txid, err := tree.Root.TXID()
		require.NoError(t, err)
		child.Input = wire.OutPoint{
			Hash:  txid,
			Index: 0,
		}
		tree.Root.Children[0] = child

		err = tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "child at output index 0")
		require.Contains(
			t, err.Error(),
			"output value 401 exceeds input",
		)
	})

	t.Run("negative batch output", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(-1, 0)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"batch output has negative value",
		)
	})

	t.Run("batch output exceeds money supply", func(t *testing.T) {
		t.Parallel()

		maxSatoshi := int64(btcutil.MaxSatoshi)
		tree := valueTestTree(maxSatoshi+1, maxSatoshi)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "batch output value")
		require.Contains(t, err.Error(), "exceeds maximum")
	})

	t.Run("negative node output", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, -1)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "output 0 has negative value")
	})

	t.Run("individual output exceeds money supply", func(t *testing.T) {
		t.Parallel()

		maxSatoshi := int64(btcutil.MaxSatoshi)
		tree := valueTestTree(maxSatoshi, maxSatoshi+1)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds maximum")
	})

	t.Run("output total exceeds money supply", func(t *testing.T) {
		t.Parallel()

		maxSatoshi := int64(btcutil.MaxSatoshi)
		tree := valueTestTree(maxSatoshi, maxSatoshi, 1)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"total output value exceeds maximum",
		)
	})

	t.Run("nil output", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		tree.Root.Outputs[0] = nil
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "output 0 is nil")
	})

	t.Run("no outputs", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000)
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "transaction has no outputs")
	})

	t.Run("child output index out of range", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		tree.Root.Children[1] = &Node{}
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"child references non-existent output index 1",
		)
	})

	t.Run("nil child", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		tree.Root.Children[0] = nil
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"child at output index 0 is nil",
		)
	})

	t.Run("child input index mismatch", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		txid, err := tree.Root.TXID()
		require.NoError(t, err)
		tree.Root.Children[0] = &Node{
			Input: wire.OutPoint{
				Hash:  txid,
				Index: 1,
			},
			Outputs: []*wire.TxOut{
				wire.NewTxOut(1000, []byte{0x51}),
			},
			Children: make(map[uint32]*Node),
		}

		err = tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "funding outpoint")
	})

	t.Run("cycle", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 1000)
		tree.Root.Children[0] = tree.Root
		err := tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "tree contains a cycle")
	})

	t.Run("multiple parents", func(t *testing.T) {
		t.Parallel()

		tree := valueTestTree(1000, 500, 500)
		txid, err := tree.Root.TXID()
		require.NoError(t, err)
		child := &Node{
			Input: wire.OutPoint{
				Hash: txid,
			},
			Outputs: []*wire.TxOut{
				wire.NewTxOut(500, []byte{0x51}),
			},
			Children: make(map[uint32]*Node),
		}
		tree.Root.Children[0] = child
		tree.Root.Children[1] = child

		err = tree.ValidateValueConservation()
		require.Error(t, err)
		require.Contains(t, err.Error(), "multiple parents")
	})
}

// TestValidateValueConservationExtractedPath verifies that both extraction
// APIs retain every parent output while omitting unrelated child nodes, so
// conservation can total all outputs while recursing through reachable
// children only.
func TestValidateValueConservationExtractedPath(t *testing.T) {
	t.Parallel()

	_, operatorKey := createTestKey(t)
	_, clientKey := createTestKey(t)
	_, otherKey := createTestKey(t)

	leaves := []LeafDescriptor{{
		PkScript:    []byte("client"),
		Amount:      1000,
		CoSignerKey: clientKey,
	}, {
		PkScript:    []byte("other"),
		Amount:      2000,
		CoSignerKey: otherKey,
	}}
	tree, err := NewTree(
		wire.OutPoint{
			Hash: chainhash.HashH([]byte("batch")),
		},
		wire.NewTxOut(
			3000, []byte("batch"),
		),
		leaves,
		operatorKey,
		make([]byte, 32),
		2,
	)
	require.NoError(t, err)

	path, err := tree.ExtractPathForCoSigners(clientKey)
	require.NoError(t, err)
	require.NotNil(t, path)
	require.Len(t, path.Root.Outputs, 3)
	require.Len(t, path.Root.Children, 1)
	require.NoError(t, path.ValidateValueConservation())

	indexPath, err := tree.ExtractPathForIndices(0)
	require.NoError(t, err)
	require.NotNil(t, indexPath)
	require.Len(t, indexPath.Root.Outputs, 3)
	require.Len(t, indexPath.Root.Children, 1)
	require.NoError(t, indexPath.ValidateValueConservation())
}
