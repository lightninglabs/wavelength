package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/stretchr/testify/require"
)

// TestManualSweepSignsRequestedPayout checks the signed Bitcoin output and
// verifies that the requested rate overrides the chain estimator.
func TestManualSweepSignsRequestedPayout(t *testing.T) {
	t.Parallel()

	proof, desc, wallet := signedSweepFixture(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	destination, err := txscript.PayToTaprootScript(key.PubKey())
	require.NoError(t, err)
	const feeRate = 7
	desc.ManualExitOptions = &actormsg.ManualExitOptions{
		SweepScript: destination, FeeRateSatPerVByte: feeRate,
	}

	sweep, err := buildSweepTx(
		t.Context(), wallet, &fakeChainSourceRef{}, proof, desc, 100, 2,
		104, NewStandardVTXOExitSpendPolicy(desc),
	)
	require.NoError(t, err)
	target, err := proof.TargetOutput()
	require.NoError(t, err)
	require.Len(t, sweep.TxOut, 1)
	require.Equal(t, destination, sweep.TxOut[0].PkScript)
	require.EqualValues(
		t, feeRate*estimatedSweepVBytes,
		target.Value-sweep.TxOut[0].Value,
	)
	require.NotEmpty(t, sweep.TxIn[0].Witness)
}

// TestManualExternalSweepDoesNotCreditWallet checks the completion ledger
// classifies the payout from its actual destination.
func TestManualExternalSweepDoesNotCreditWallet(t *testing.T) {
	t.Parallel()

	proof, desc, wallet := signedSweepFixture(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	destination, err := txscript.PayToTaprootScript(key.PubKey())
	require.NoError(t, err)
	desc.ManualExitOptions = &actormsg.ManualExitOptions{
		SweepScript: destination, FeeRateSatPerVByte: 7,
	}
	tx, err := buildSweepTx(
		t.Context(), wallet, &fakeChainSourceRef{}, proof, desc, 100, 2,
		104, NewStandardVTXOExitSpendPolicy(desc),
	)
	require.NoError(t, err)
	b := &behavior{proof: proof, desc: desc, sweepTx: tx}

	owned, err := b.exitPaysWallet(t.Context())
	require.NoError(t, err)
	msg, err := b.exitCostMsg(&JobState{}, owned)
	require.NoError(t, err)
	require.False(t, msg.DestinationOwnWallet)
}
