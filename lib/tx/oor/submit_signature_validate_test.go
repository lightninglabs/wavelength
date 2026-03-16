package oor

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestValidateSubmitPackageSignedHappyPath asserts a signed submit package
// with valid tapscript data passes full validation.
func TestValidateSubmitPackageSignedHappyPath(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
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

	policy := arkscript.CheckpointPolicy{
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

// TestValidateFinalizePackageSignedAllowsMissingInputTapTree asserts finalize
// signed validation accepts checkpoint inputs without taptree unknown metadata
// when FinalScriptWitness is present and spendable.
func TestValidateFinalizePackageSignedAllowsMissingInputTapTree(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{3},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: p2WSHTrueScript(),
			},
		},
		OwnerLeafScript: []byte{txscript.OP_TRUE},
	})
	require.NoError(t, err)

	finalWitness, err := encodeFinalWitness(
		wire.TxWitness{[]byte{txscript.OP_TRUE}},
	)
	require.NoError(t, err)
	checkpointRes.PSBT.Inputs[0].FinalScriptWitness = finalWitness
	checkpointRes.PSBT.Inputs[0].Unknowns = nil

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(5000),
	}})
	require.NoError(t, err)

	err = ValidateFinalizePackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.NoError(t, err)
}

// TestValidateFinalizeSignedRejectsMalformedInputTapTree asserts
// malformed taptree input metadata remains a hard validation error.
func TestValidateFinalizeSignedRejectsMalformedInputTapTree(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{4},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: p2WSHTrueScript(),
			},
		},
		OwnerLeafScript: []byte{txscript.OP_TRUE},
	})
	require.NoError(t, err)

	finalWitness, err := encodeFinalWitness(
		wire.TxWitness{[]byte{txscript.OP_TRUE}},
	)
	require.NoError(t, err)
	checkpointRes.PSBT.Inputs[0].FinalScriptWitness = finalWitness
	checkpointRes.PSBT.Inputs[0].Unknowns = []*psbt.Unknown{
		{
			Key:   append([]byte(nil), TapTreePSBTKey...),
			Value: []byte{0x01},
		},
		{
			Key:   append([]byte(nil), TapTreePSBTKey...),
			Value: []byte{0x02},
		},
	}

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(5000),
	}})
	require.NoError(t, err)

	err = ValidateFinalizePackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.ErrorContains(t, err, "multiple tap tree entries found")
}

// TestValidateSubmitSignedRejectsMissingArkInputTapTree asserts submit
// signed validation still enforces required Ark-input taptree metadata.
func TestValidateSubmitSignedRejectsMissingArkInputTapTree(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{5},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: randomP2TRScript(t),
			},
		},
		OwnerLeafScript: []byte{txscript.OP_TRUE},
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

	arkPSBT.Inputs[0].Unknowns = nil

	_, err = ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.ErrorContains(t, err, "missing tap tree metadata")
}

func p2WSHTrueScript() []byte {
	scriptHash := sha256.Sum256([]byte{txscript.OP_TRUE})
	return append([]byte{txscript.OP_0, 0x20}, scriptHash[:]...)
}

func encodeFinalWitness(wit wire.TxWitness) ([]byte, error) {
	var b bytes.Buffer

	err := wire.WriteVarInt(&b, 0, uint64(len(wit)))
	if err != nil {
		return nil, err
	}

	for i := range wit {
		err = wire.WriteVarBytes(&b, 0, wit[i])
		if err != nil {
			return nil, err
		}
	}

	return b.Bytes(), nil
}
