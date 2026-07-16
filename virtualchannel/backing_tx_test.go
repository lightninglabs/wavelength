package virtualchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestBuildBackingTx sorts multi-VTXO inputs and returns the implicit fee.
func TestBuildBackingTx(t *testing.T) {
	t.Parallel()

	first := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("a")),
		Index: 2,
	}
	second := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("b")),
		Index: 1,
	}

	tx, fee, err := BuildBackingTx(
		[]BackingVTXO{
			{
				OutPoint: second,
				Amount:   btcutil.Amount(30_000),
			},
			{
				OutPoint: first,
				Amount:   btcutil.Amount(70_000),
			},
		},
		&wire.TxOut{
			Value:    99_000,
			PkScript: []byte{0x51, 0x20},
		},
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(1_000), fee)
	require.Len(t, tx.TxIn, 2)
	require.Equal(t, []wire.OutPoint{second, first}, []wire.OutPoint{
		tx.TxIn[0].PreviousOutPoint,
		tx.TxIn[1].PreviousOutPoint,
	})
	require.Equal(t, wire.MaxTxInSequenceNum, tx.TxIn[0].Sequence)
	require.Equal(t, wire.MaxTxInSequenceNum, tx.TxIn[1].Sequence)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, int64(99_000), tx.TxOut[0].Value)
	require.Equal(t, []byte{0x51, 0x20}, tx.TxOut[0].PkScript)
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
