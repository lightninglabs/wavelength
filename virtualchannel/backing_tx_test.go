package virtualchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestBuildBackingTx binds one VTXO and returns the implicit fee.
func TestBuildBackingTx(t *testing.T) {
	t.Parallel()

	backingOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("a")),
		Index: 2,
	}

	tx, fee, err := BuildBackingTx(
		[]BackingVTXO{{
			OutPoint: backingOutpoint,
			Amount:   btcutil.Amount(100_000),
		}},
		&wire.TxOut{
			Value:    99_000,
			PkScript: []byte{0x51, 0x20},
		},
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(1_000), fee)
	require.Len(t, tx.TxIn, 1)
	require.Equal(t, backingOutpoint, tx.TxIn[0].PreviousOutPoint)
	require.Equal(t, wire.MaxTxInSequenceNum, tx.TxIn[0].Sequence)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, int64(99_000), tx.TxOut[0].Value)
	require.Equal(t, []byte{0x51, 0x20}, tx.TxOut[0].PkScript)
}

func TestBuildBackingTxRejectsMultipleVTXOs(t *testing.T) {
	_, _, err := BuildBackingTx(
		[]BackingVTXO{
			{Amount: 60_000},
			{Amount: 40_000},
		}, &wire.TxOut{
			Value:    99_000,
			PkScript: []byte{0x51},
		},
	)
	require.ErrorContains(t, err, "exactly one")
}

// TestBuildBackingTxRejectsUnderfundedOutput prevents invalid funding parents.
func TestBuildBackingTxRejectsUnderfundedOutput(t *testing.T) {
	t.Parallel()

	_, _, err := BuildBackingTx(
		[]BackingVTXO{
			{
				OutPoint: wire.OutPoint{
					Hash:  chainhash.HashH([]byte("a")),
					Index: 0,
				},
				Amount: btcutil.Amount(10_000),
			},
		},
		&wire.TxOut{
			Value:    11_000,
			PkScript: []byte{0x51},
		},
	)
	require.ErrorContains(t, err, "below funding output")
}

func TestBuildBackingTxRejectsMoneySupplyOverflow(t *testing.T) {
	t.Parallel()

	_, _, err := BuildBackingTx(
		[]BackingVTXO{
			{Amount: btcutil.MaxSatoshi + 1},
		}, &wire.TxOut{
			Value:    1,
			PkScript: []byte{0x51},
		},
	)
	require.ErrorContains(t, err, "money supply")

	_, _, err = BuildBackingTx(
		[]BackingVTXO{{Amount: btcutil.MaxSatoshi}},
		&wire.TxOut{
			Value:    int64(btcutil.MaxSatoshi) + 1,
			PkScript: []byte{0x51},
		},
	)
	require.ErrorContains(t, err, "money supply")
}
