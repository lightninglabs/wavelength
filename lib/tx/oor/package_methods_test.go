package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestSubmitPackageValidateHappyPath asserts the SubmitPackage method wrapper
// applies the shared structural submit validator.
func TestSubmitPackageValidateHappyPath(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	cpResult, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{1},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: randomP2TRScript(t),
			},
		},
		OwnerLeafScript: []byte{
			txscript.OP_1,
			txscript.OP_1,
			txscript.OP_ADD,
			txscript.OP_2,
			txscript.OP_EQUAL,
		},
	})
	require.NoError(t, err)

	checkpointTx := cpResult.PSBT.UnsignedTx
	require.NotNil(t, checkpointTx)

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{
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

	pkg := &SubmitPackage{
		ArkPSBT: arkPSBT,
		CheckpointPSBTs: []*psbt.Packet{
			cpResult.PSBT,
		},
	}

	validated, err := pkg.Validate()
	require.NoError(t, err)
	require.NotNil(t, validated)
	require.Len(t, validated.CheckpointOutpoints, 1)
}

// TestSubmitPackageValidateRejectsNil asserts nil receivers are rejected.
func TestSubmitPackageValidateRejectsNil(t *testing.T) {
	t.Parallel()

	var pkg *SubmitPackage
	_, err := pkg.Validate()
	require.Error(t, err)
}

// TestFinalizePackageValidateHappyPath asserts the FinalizePackage method
// wrapper applies the shared structural finalize validator.
func TestFinalizePackageValidateHappyPath(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	cpResult, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{1},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: randomP2TRScript(t),
			},
		},
		OwnerLeafScript: []byte{txscript.OP_1},
	})
	require.NoError(t, err)

	checkpointTx := cpResult.PSBT.UnsignedTx
	require.NotNil(t, checkpointTx)

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{
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

	finalCheckpoint := cpResult.PSBT
	finalCheckpoint.Inputs[0].TaprootScriptSpendSig =
		[]*psbt.TaprootScriptSpendSig{
			{},
		}

	pkg := &FinalizePackage{
		ArkPSBT: arkPSBT,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
	}

	err = pkg.Validate()
	require.NoError(t, err)
}

// TestFinalizePackageValidateRejectsNil asserts nil receivers are rejected.
func TestFinalizePackageValidateRejectsNil(t *testing.T) {
	t.Parallel()

	var pkg *FinalizePackage
	err := pkg.Validate()
	require.Error(t, err)
}
