package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// newStructuralCheckpoint builds a minimal checkpoint PSBT with an anchor
// output, spending the given prevout hash. If finalWitness is set, a synthetic
// final script witness is attached so the checkpoint counts as signed.
func newStructuralCheckpoint(t *testing.T, prevHash chainhash.Hash,
	prevIndex uint32, finalWitness bool) *psbt.Packet {

	t.Helper()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  prevHash,
			Index: prevIndex,
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
	if finalWitness {
		checkpointPsbt.Inputs[0].FinalScriptWitness = []byte{0x01}
	}

	return checkpointPsbt
}

// newStructuralArkPSBT builds a minimal Ark PSBT spending checkpoint output 0
// of the given checkpoint transaction.
func newStructuralArkPSBT(t *testing.T,
	checkpointPsbt *psbt.Packet) *psbt.Packet {

	t.Helper()

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointPsbt.UnsignedTx.TxHash(),
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

	return arkPsbt
}

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
	checkpointPsbt := newStructuralCheckpoint(
		t, chainhash.Hash{1}, 7, true,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbt)

	err := ValidateFinalizePackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.NoError(t, err)
}

// TestValidateFinalizePackageMissingSig asserts we reject finalize checkpoints
// that do not include any signature material.
func TestValidateFinalizePackageMissingSig(t *testing.T) {
	t.Parallel()

	checkpointPsbt := newStructuralCheckpoint(
		t, chainhash.Hash{1}, 7, false,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbt)

	err := ValidateFinalizePackage(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}

// TestValidateFinalizePackageExtraCheckpoint asserts the checkpoint set must
// match the Ark input set exactly.
func TestValidateFinalizePackageExtraCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointPsbtA := newStructuralCheckpoint(
		t, chainhash.Hash{1}, 7, true,
	)
	checkpointPsbtB := newStructuralCheckpoint(
		t, chainhash.Hash{2}, 9, true,
	)
	arkPsbt := newStructuralArkPSBT(t, checkpointPsbtA)

	err := ValidateFinalizePackage(arkPsbt, []*psbt.Packet{
		checkpointPsbtA,
		checkpointPsbtB,
	})
	require.Error(t, err)
}
