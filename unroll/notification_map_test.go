package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/stretchr/testify/require"
)

// TestMapTxconfirmNotification pins the disposition of every txconfirm
// lifecycle event at the unroll subscriber boundary. The load-bearing
// cases are TxFinalized and TxReorged: this planner has durable transitions
// for both, so the adapter must preserve rather than drop or collapse them.
func TestMapTxconfirmNotification(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{0xaa}

	t.Run("confirmed maps to confirmation", func(t *testing.T) {
		t.Parallel()

		msg, ok := mapTxconfirmNotification(&txconfirm.TxConfirmed{
			Txid:        txid,
			BlockHeight: 123,
			NumConfs:    3,
		})
		require.True(t, ok)

		confirmed, isConfirmed := msg.(*TxConfirmedMsg)
		require.True(t, isConfirmed)
		require.Equal(t, txid, confirmed.Txid)
		require.EqualValues(t, 123, confirmed.Height)
		require.EqualValues(t, 3, confirmed.NumConfs)
	})

	t.Run("finalized maps to finality", func(t *testing.T) {
		t.Parallel()

		msg, ok := mapTxconfirmNotification(&txconfirm.TxFinalized{
			Txid:        txid,
			BlockHeight: 130,
			NumConfs:    6,
		})
		require.True(t, ok)

		finalized, isFinalized := msg.(*TxFinalizedMsg)
		require.True(t, isFinalized)
		require.Equal(t, txid, finalized.Txid)
	})

	t.Run("reorged maps to reorg", func(t *testing.T) {
		t.Parallel()

		msg, ok := mapTxconfirmNotification(&txconfirm.TxReorged{
			Txid: txid,
		})
		require.True(t, ok)
		reorged, isReorged := msg.(*TxReorgedMsg)
		require.True(t, isReorged)
		require.Equal(t, txid, reorged.Txid)
	})

	t.Run("failed maps to failure", func(t *testing.T) {
		t.Parallel()

		msg, ok := mapTxconfirmNotification(&txconfirm.TxFailed{
			Txid:   txid,
			Reason: "broadcast rejected",
		})
		require.True(t, ok)

		failed, isFailed := msg.(*TxFailedMsg)
		require.True(t, isFailed)
		require.Equal(t, txid, failed.Txid)
		require.Equal(t, "broadcast rejected", failed.Reason)
	})
}
