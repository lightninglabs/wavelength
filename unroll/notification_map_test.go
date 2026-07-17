package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/stretchr/testify/require"
)

// TestMapTxconfirmNotification pins the disposition of every txconfirm
// lifecycle event at the unroll subscriber boundary. The load-bearing
// cases are TxFinalized and TxReorged: subscribers receive the FULL
// reorg-aware lifecycle, and before this mapping existed both fell into
// the unknown-notification arm and terminally failed the unroll job —
// finality (synthesized by every non-lnd backend once the confirmation
// passed the reorg-safety depth) killed otherwise-healthy exits.
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

	t.Run("finalized maps to confirmation, not failure", func(
		t *testing.T) {

		t.Parallel()

		msg, ok := mapTxconfirmNotification(&txconfirm.TxFinalized{
			Txid:        txid,
			BlockHeight: 130,
			NumConfs:    6,
		})
		require.True(t, ok)

		confirmed, isConfirmed := msg.(*TxConfirmedMsg)
		require.True(
			t, isConfirmed, "finality must replay a "+
				"confirmation; mapping it to a failure "+
				"terminally kills the unroll job",
		)
		require.Equal(t, txid, confirmed.Txid)
		require.EqualValues(t, 130, confirmed.Height)
		require.EqualValues(t, 6, confirmed.NumConfs)
	})

	t.Run("reorged is dropped", func(t *testing.T) {
		t.Parallel()

		_, ok := mapTxconfirmNotification(&txconfirm.TxReorged{
			Txid: txid,
		})
		require.False(
			t, ok, "TxReorged is best-effort and superseded by "+
				"the next reliable event; it must not "+
				"become a failure",
		)
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
