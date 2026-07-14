package checkpoint

import (
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/stretchr/testify/require"
)

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

// TestBuildPSBTHappyPath asserts BuildPSBT creates a signable checkpoint PSBT
// and returns the corresponding tap tree encoding.
func TestBuildPSBTHappyPath(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	witnessUtxo := &wire.TxOut{
		Value:    5000,
		PkScript: randomP2TRScript(t),
	}

	in := Input{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash: [32]byte{
					1,
				},
				Index: 0,
			},
			Output: witnessUtxo,
		},
		OwnerLeafScript: []byte{
			txscript.OP_1,
			txscript.OP_1,
			txscript.OP_ADD,
			txscript.OP_2,
			txscript.OP_EQUAL,
		},
	}

	result, err := BuildPSBT(policy, in)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.PSBT)
	require.NotNil(t, result.PSBT.UnsignedTx)

	tx := result.PSBT.UnsignedTx
	require.Equal(t, int32(arktx.TxVersion), tx.Version)
	require.Len(t, tx.TxIn, 1)
	require.Equal(t, in.SpentVTXO.Outpoint, tx.TxIn[0].PreviousOutPoint)
	require.Len(t, tx.TxOut, 2)
	require.Equal(t, witnessUtxo.Value, tx.TxOut[0].Value)
	require.True(t, arktx.IsAnchorOutput(tx.TxOut[1]))

	expectedPkScript, err := arkscript.CheckpointPkScript(
		policy, in.OwnerLeafScript,
	)
	require.NoError(t, err)
	require.Equal(t, expectedPkScript, tx.TxOut[0].PkScript)

	require.NotNil(t, result.PSBT.Inputs[0].WitnessUtxo)
	require.Equal(t, witnessUtxo, result.PSBT.Inputs[0].WitnessUtxo)

	decoded, err := DecodeTapTree(result.TapTreeEncoded)
	require.NoError(t, err)

	tapscript, err := arkscript.CheckpointTapScript(
		policy, in.OwnerLeafScript,
	)
	require.NoError(t, err)

	expectedLeaves := make([][]byte, 0, len(tapscript.Leaves))
	for _, leaf := range tapscript.Leaves {
		expectedLeaves = append(expectedLeaves, leaf.Script)
	}

	require.Equal(t, expectedLeaves, decoded)
}

// TestBuildPSBTRejectsMissingWitness asserts missing witness data is rejected.
func TestBuildPSBTRejectsMissingWitness(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	_, err = BuildPSBT(policy, Input{})
	require.Error(t, err)
}
