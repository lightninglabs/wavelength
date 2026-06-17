package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// newSubmitCheckpoint builds a checkpoint PSBT for submit validation: a single
// output (value 1234, pkScript 0x51) plus, when anchor is set, the canonical
// anchor output and a TaprootTapTree sidecar. It returns both the PSBT and the
// spendable output for use as an Ark input witness UTXO.
func newSubmitCheckpoint(t *testing.T, prevHash chainhash.Hash,
	prevIndex uint32, anchor bool) (*psbt.Packet, *wire.TxOut) {

	t.Helper()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  prevHash,
			Index: prevIndex,
		},
	})

	checkpointOut := &wire.TxOut{
		Value: 1234,
		PkScript: []byte{
			0x51,
		},
	}
	checkpointTx.AddTxOut(checkpointOut)
	if anchor {
		checkpointTx.AddTxOut(arkscript.AnchorOutput())
	}

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	if anchor {
		checkpointPsbt.Outputs = []psbt.POutput{{
			TaprootTapTree: []byte{
				0x51,
			},
		}}
	}

	return checkpointPsbt, checkpointOut
}

// TestValidateSubmitPackageHappyPath asserts a well-formed submit package
// validates successfully and produces derived mapping info.
func TestValidateSubmitPackageHappyPath(t *testing.T) {
	t.Parallel()

	checkpointPsbt, checkpointOut := newSubmitCheckpoint(
		t, chainhash.Hash{1}, 7, true,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbt)
	arkPsbt.Inputs[0].WitnessUtxo = checkpointOut

	validated, err := ValidateSubmitPackage(
		arkPsbt, []*psbt.Packet{checkpointPsbt},
	)
	require.NoError(t, err)
	require.NotNil(t, validated)
	require.Equal(t, arkPsbt.UnsignedTx.TxHash(), validated.ArkTxid)
	require.Len(t, validated.CheckpointOutpoints, 1)
	require.Equal(
		t, arkPsbt.UnsignedTx.TxIn[0].PreviousOutPoint,
		validated.CheckpointOutpoints[0],
	)
}

// TestValidateSubmitPackageMissingWitness asserts we reject if Ark PSBT inputs
// don't carry witness UTXOs.
func TestValidateSubmitPackageMissingWitness(t *testing.T) {
	t.Parallel()

	checkpointPsbt, _ := newSubmitCheckpoint(
		t, chainhash.Hash{1}, 7, true,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbt)

	_, err := ValidateSubmitPackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}

// TestValidateSubmitPackageExtraCheckpoint asserts we reject if the caller
// supplies checkpoint PSBTs that don't match the Ark inputs.
func TestValidateSubmitPackageExtraCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointPsbtA, checkpointOutA := newSubmitCheckpoint(
		t, chainhash.Hash{1}, 7, true,
	)
	checkpointPsbtB, _ := newSubmitCheckpoint(
		t, chainhash.Hash{2}, 9, true,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbtA)
	arkPsbt.Inputs[0].WitnessUtxo = checkpointOutA

	_, err := ValidateSubmitPackage(arkPsbt, []*psbt.Packet{
		checkpointPsbtA,
		checkpointPsbtB,
	})
	require.Error(t, err)
}

// TestValidateSubmitPackageRejectsCheckpointWithoutAnchor asserts old-style
// anchorless checkpoints fail with a clear structural error.
func TestValidateSubmitPackageRejectsCheckpointWithoutAnchor(t *testing.T) {
	t.Parallel()

	checkpointPsbt, checkpointOut := newSubmitCheckpoint(
		t, chainhash.Hash{1}, 7, false,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbt)
	arkPsbt.Inputs[0].WitnessUtxo = checkpointOut

	_, err := ValidateSubmitPackage(
		arkPsbt, []*psbt.Packet{checkpointPsbt},
	)
	require.ErrorContains(t, err, "anchor output")
}
