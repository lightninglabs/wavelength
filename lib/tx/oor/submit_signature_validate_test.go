package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/stretchr/testify/require"
)

// TestValidateSubmitPackageSignedHappyPath asserts a signed submit package
// with valid tapscript data passes full validation.
func TestValidateSubmitPackageSignedHappyPath(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	ownerLeafScript := []byte{txscript.OP_TRUE}
	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
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
		OwnerLeafScript: ownerLeafScript,
	})
	require.NoError(t, err)

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(5000),
	}})
	require.NoError(t, err)

	leaf, err := BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeafScript,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	_, err = ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.NoError(t, err)
}

// TestValidateSubmitPackageSignedRejectsBadControlBlock asserts a tampered
// control block fails full validation.
func TestValidateSubmitPackageSignedRejectsBadControlBlock(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	ownerLeafScript := []byte{txscript.OP_TRUE}
	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{2},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: randomP2TRScript(t),
			},
		},
		OwnerLeafScript: ownerLeafScript,
	})
	require.NoError(t, err)

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(5000),
	}})
	require.NoError(t, err)

	leaf, err := BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeafScript,
	)
	require.NoError(t, err)
	leaf.ControlBlock[0] ^= 0x01
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	_, err = ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.Error(t, err)
}
