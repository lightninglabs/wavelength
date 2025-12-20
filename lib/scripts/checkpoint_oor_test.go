package scripts

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// TestCheckpointPkScriptIsTaproot asserts that the checkpoint helper returns a
// valid P2TR script and that it binds the expected internal key and tap tree.
func TestCheckpointPkScriptIsTaproot(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	ownerLeafScript := []byte{
		txscript.OP_1,
		txscript.OP_1,
		txscript.OP_ADD,
		txscript.OP_2,
		txscript.OP_EQUAL,
	}

	tapscript, err := CheckpointTapScript(policy, ownerLeafScript)
	require.NoError(t, err)
	require.NotNil(t, tapscript)

	pkScript, err := CheckpointPkScript(policy, ownerLeafScript)
	require.NoError(t, err)
	require.True(t, txscript.IsPayToTaproot(pkScript))

	tree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	expectedRoot := tree.RootNode.TapHash()
	require.Equal(t, expectedRoot[:], tapscript.RootHash)

	expectedKey := txscript.ComputeTaprootOutputKey(
		&ARKNUMSKey, tapscript.RootHash,
	)

	actualKey, err := tapscript.TaprootKey()
	require.NoError(t, err)
	require.Equal(t, expectedKey.SerializeCompressed(),
		actualKey.SerializeCompressed())
}
