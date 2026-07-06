package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestValidateSubmitPackageHappyPath asserts a well-formed submit package
// validates successfully and produces derived mapping info.
func TestValidateSubmitPackageHappyPath(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})

	checkpointOut := &wire.TxOut{
		Value: 1234,
		PkScript: []byte{
			0x51,
		},
	}
	checkpointTx.AddTxOut(checkpointOut)
	checkpointTx.AddTxOut(arkscript.AnchorOutput())

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	checkpointPsbt.Outputs = []psbt.POutput{{
		TaprootTapTree: []byte{
			0x51,
		},
	}}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	arkPsbt.Inputs[0].WitnessUtxo = checkpointOut

	validated, err := ValidateSubmitPackage(
		arkPsbt, []*psbt.Packet{checkpointPsbt},
	)
	require.NoError(t, err)
	require.NotNil(t, validated)
	require.Equal(t, arkTx.TxHash(), validated.ArkTxid)
	require.Len(t, validated.CheckpointOutpoints, 1)
	require.Equal(
		t, arkTx.TxIn[0].PreviousOutPoint,
		validated.CheckpointOutpoints[0],
	)
}

// TestValidateSubmitPackageMissingWitness asserts we reject if Ark PSBT inputs
// don't carry witness UTXOs.
func TestValidateSubmitPackageMissingWitness(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})
	checkpointTx.AddTxOut(arkscript.AnchorOutput())

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	checkpointPsbt.Outputs = []psbt.POutput{{
		TaprootTapTree: []byte{
			0x51,
		},
	}}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	_, err = ValidateSubmitPackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}

// TestValidateSubmitPackageExtraCheckpoint asserts we reject if the caller
// supplies checkpoint PSBTs that don't match the Ark inputs.
func TestValidateSubmitPackageExtraCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointTxA := wire.NewMsgTx(3)
	checkpointTxA.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointOutA := &wire.TxOut{
		Value: 1234,
		PkScript: []byte{
			0x51,
		},
	}
	checkpointTxA.AddTxOut(checkpointOutA)
	checkpointTxA.AddTxOut(arkscript.AnchorOutput())

	checkpointPsbtA, err := psbt.NewFromUnsignedTx(checkpointTxA)
	require.NoError(t, err)
	checkpointPsbtA.Outputs = []psbt.POutput{{
		TaprootTapTree: []byte{
			0x51,
		},
	}}

	checkpointTxB := wire.NewMsgTx(3)
	checkpointTxB.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 9,
		},
	})
	checkpointTxB.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})
	checkpointTxB.AddTxOut(arkscript.AnchorOutput())

	checkpointPsbtB, err := psbt.NewFromUnsignedTx(checkpointTxB)
	require.NoError(t, err)
	checkpointPsbtB.Outputs = []psbt.POutput{{
		TaprootTapTree: []byte{
			0x51,
		},
	}}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTxA.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	arkPsbt.Inputs[0].WitnessUtxo = checkpointOutA

	_, err = ValidateSubmitPackage(arkPsbt, []*psbt.Packet{
		checkpointPsbtA,
		checkpointPsbtB,
	})
	require.Error(t, err)
}

// TestValidateSubmitPackageRejectsCheckpointWithoutAnchor asserts old-style
// anchorless checkpoints fail with a clear structural error.
func TestValidateSubmitPackageRejectsCheckpointWithoutAnchor(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})

	checkpointOut := &wire.TxOut{
		Value: 1234,
		PkScript: []byte{
			0x51,
		},
	}
	checkpointTx.AddTxOut(checkpointOut)

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	arkPsbt.Inputs[0].WitnessUtxo = checkpointOut

	_, err = ValidateSubmitPackage(
		arkPsbt, []*psbt.Packet{checkpointPsbt},
	)
	require.ErrorContains(t, err, "anchor output")
}
