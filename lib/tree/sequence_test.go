package tree

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestNodeTxSequence pins both flow sequences: the zero value keeps the V1
// final sequence, an explicit SequenceV2 signals RBF, and the two produce
// different txids because the sequence is consensus-visible.
func TestNodeTxSequence(t *testing.T) {
	t.Parallel()

	input := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("parent")),
		Index: 0,
	}
	outputs := []*wire.TxOut{{Value: 1_000, PkScript: []byte{0x51}}}

	v1 := &Node{Input: input, Outputs: outputs}
	v2 := &Node{Input: input, Outputs: outputs, Sequence: SequenceV2}

	require.Equal(t, wire.MaxTxInSequenceNum, v1.TxSequence())
	require.Equal(t, SequenceV2, v2.TxSequence())
	require.Equal(t, wire.MaxTxInSequenceNum-2, v2.TxSequence())

	v1Tx, err := v1.ToTx()
	require.NoError(t, err)
	v2Tx, err := v2.ToTx()
	require.NoError(t, err)
	require.NotEqual(t, v1Tx.TxHash(), v2Tx.TxHash())
}
