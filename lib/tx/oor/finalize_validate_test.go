package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestValidateFinalizePackageHappyPath asserts a structurally valid finalize
// package is accepted.
func TestValidateFinalizePackageHappyPath(t *testing.T) {
	t.Parallel()

	// Finalize validation is intentionally shallow:
	// - We only require that every checkpoint has some final witness.
	// - Signature correctness is out of scope for v0 structural checks.
	//
	// This keeps the server-side validation lightweight and allows tests
	// to use synthetic signatures.
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

	// For structural validation, a non-empty final witness is sufficient.
	checkpointPsbt.Inputs[0].FinalScriptWitness = []byte{0x01}

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

	err = ValidateFinalizePackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.NoError(t, err)
}

// TestValidateFinalizePackageMissingSig asserts we reject finalize checkpoints
// that do not include any signature material.
func TestValidateFinalizePackageMissingSig(t *testing.T) {
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

	err = ValidateFinalizePackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}

// TestValidateFinalizePackageExtraCheckpoint asserts the checkpoint set must
// match the Ark input set exactly.
func TestValidateFinalizePackageExtraCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointTxA := wire.NewMsgTx(3)
	checkpointTxA.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTxA.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})
	checkpointTxA.AddTxOut(arkscript.AnchorOutput())

	checkpointPsbtA, err := psbt.NewFromUnsignedTx(checkpointTxA)
	require.NoError(t, err)
	checkpointPsbtA.Inputs[0].FinalScriptWitness = []byte{0x01}

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
	checkpointPsbtB.Inputs[0].FinalScriptWitness = []byte{0x01}

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

	err = ValidateFinalizePackage(arkPsbt, []*psbt.Packet{
		checkpointPsbtA,
		checkpointPsbtB,
	})
	require.Error(t, err)
}
