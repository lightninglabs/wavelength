package oor

import (
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

// TestBuildCheckpointAndArkPSBT asserts the builders produce a submit package
// that passes the shared submit validator.
func TestBuildCheckpointAndArkPSBT(t *testing.T) {
	t.Parallel()

	// This is an integration-style unit test over the tx builder layer:
	// - BuildCheckpointPSBT produces a checkpoint spend for a single VTXO.
	// - BuildArkPSBT consumes the checkpoint output and adds recipients +
	//   anchor output.
	// - ValidateSubmitPackage then enforces the shared structural rules.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	vtxoWitness := &wire.TxOut{
		Value:    5000,
		PkScript: randomP2TRScript(t),
	}

	ownerLeafScript := []byte{
		txscript.OP_1,
		txscript.OP_1,
		txscript.OP_ADD,
		txscript.OP_2,
		txscript.OP_EQUAL,
	}

	cpResult, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1},
				Index: 0,
			},
			Output: vtxoWitness,
		},
		OwnerLeafScript: ownerLeafScript,
	})
	require.NoError(t, err)
	require.NotNil(t, cpResult)

	checkpointTx := cpResult.PSBT.UnsignedTx
	require.NotNil(t, checkpointTx)
	require.Len(t, checkpointTx.TxOut, 1)

	arkPsbt, err := BuildArkPSBT([]CheckpointOutput{
		{
			Txid:           checkpointTx.TxHash(),
			Output:         checkpointTx.TxOut[0],
			TapTreeEncoded: cpResult.TapTreeEncoded,
		},
	}, []RecipientOutput{
		{
			PkScript: randomP2TRScript(t),
			Value:    5000,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, arkPsbt)

	_, err = ValidateSubmitPackage(arkPsbt, []*psbt.Packet{cpResult.PSBT})
	require.NoError(t, err)
}
