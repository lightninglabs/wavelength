package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestBuildSignedExitSweep commits the complete payout and anchor before any
// chain observation, while using only the VTXO signing key and no fee inputs.
func TestBuildSignedExitSweep(t *testing.T) {
	t.Parallel()
	proof, desc, signer := signedSweepFixture(t)
	destinationKey, _ := btcec.PrivKeyFromBytes([]byte{3})
	destination, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(
			destinationKey.PubKey(),
		),
	)
	require.NoError(t, err)
	sweep, err := BuildSignedExitSweep(desc, proof, destination, signer)
	require.NoError(t, err)
	require.Equal(t, int32(3), sweep.Version)
	require.Zero(t, sweep.LockTime)
	require.Len(t, sweep.TxIn, 1)
	require.Equal(t, desc.Outpoint, sweep.TxIn[0].PreviousOutPoint)
	require.Equal(t, desc.RelativeExpiry, sweep.TxIn[0].Sequence)
	require.Len(t, sweep.TxOut, 2)
	require.Equal(t, int64(desc.Amount), sweep.TxOut[0].Value)
	require.Equal(t, destination, sweep.TxOut[0].PkScript)
	require.Zero(t, sweep.TxOut[1].Value)
	require.Equal(t, arkscript.AnchorPkScript, sweep.TxOut[1].PkScript)
	require.Len(t, sweep.TxIn[0].Witness, 3)
	require.Len(t, sweep.TxIn[0].Witness[0], 64)
	target, err := proof.TargetOutput()
	require.NoError(t, err)
	for _, mutate := range []struct {
		name  string
		apply func(*wire.MsgTx)
	}{
		{"destination", func(tx *wire.MsgTx) {
			tx.TxOut[0].PkScript = desc.PkScript
		}},
		{
			"amount",
			func(tx *wire.MsgTx) {
				tx.TxOut[0].Value--
			},
		},
		{
			"anchor",
			func(tx *wire.MsgTx) {
				tx.TxOut = tx.TxOut[:1]
			},
		},
		{
			"delay",
			func(tx *wire.MsgTx) {
				tx.TxIn[0].Sequence++
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			tx := sweep.Copy()
			mutate.apply(tx)
			require.Error(
				t,
				verifyTxWitnesses(
					tx, map[wire.OutPoint]*wire.TxOut{
						desc.Outpoint: target,
					},
				),
			)
		})
	}
}

// TestBuildSignedExitSweepRejectsUnsafe refuses asset carriers, mismatched
// local descriptors, unspendable destinations, and a signer returning a bad
// signature. An ancestry-only export does not call this signing boundary.
func TestBuildSignedExitSweepRejectsUnsafe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*vtxo.Descriptor)
	}{
		{"asset", func(d *vtxo.Descriptor) {
			d.TaprootAssetRoot = &chainhash.Hash{
				1,
			}
		}},
		{
			"spent",
			func(d *vtxo.Descriptor) {
				d.Status = vtxo.VTXOStatusSpent
			},
		},
		{
			"amount",
			func(d *vtxo.Descriptor) {
				d.Amount--
			},
		},
		{
			"script",
			func(d *vtxo.Descriptor) {
				d.PkScript = nil
			},
		},
		{
			"outpoint",
			func(d *vtxo.Descriptor) {
				d.Outpoint.Index++
			},
		},
		{
			"delay",
			func(d *vtxo.Descriptor) {
				d.RelativeExpiry++
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proof, desc, signer := signedSweepFixture(t)
			tc.mutate(desc)
			sweep, err := BuildSignedExitSweep(
				desc, proof, desc.PkScript, signer,
			)
			require.Error(t, err)
			require.Nil(t, sweep)
			if tc.name == "asset" {
				require.ErrorIs(
					t, err,
					vtxo.ErrAssetVTXORequiresTransition,
				)
			}
		})
	}
	proof, desc, signer := signedSweepFixture(t)
	for _, destination := range [][]byte{
		nil, arkscript.AnchorPkScript, {
			txscript.OP_RETURN,
		},
	} {
		_, err := BuildSignedExitSweep(desc, proof, destination, signer)
		require.Error(t, err)
	}
	_, err := BuildSignedExitSweep(
		desc, proof, desc.PkScript, &fakeSweepWallet{},
	)
	require.ErrorContains(t, err, "verify signed exit sweep")
}
