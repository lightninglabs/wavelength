package unroll

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestFailureClassDurableRoundTrip proves the rejection disposition survives
// the txconfirm adapter and durable mailbox, while old records stay unknown.
func TestFailureClassDurableRoundTrip(t *testing.T) {
	for _, class := range []txconfirm.BroadcastFailureClass{
		txconfirm.BroadcastFailureUnknown,
		txconfirm.BroadcastFailureFee,
		txconfirm.BroadcastFailurePermanent,
	} {
		msg, ok := mapTxconfirmNotification(&txconfirm.TxFailed{
			Class: class, Reason: "backend rejected transaction",
		})
		require.True(t, ok)
		var buf bytes.Buffer
		failed, ok := msg.(*TxFailedMsg)
		require.True(t, ok)
		require.NoError(t, failed.Encode(&buf))
		var restored TxFailedMsg
		require.NoError(t, restored.Decode(&buf))
		require.Equal(t, class, restored.Class)
	}

	var txid [32]byte
	reason := []byte("legacy failure")
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(txFailedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(txFailedReasonRecType, &reason),
	)
	require.NoError(t, err)
	var legacy bytes.Buffer
	require.NoError(t, stream.Encode(&legacy))
	var restored TxFailedMsg
	require.NoError(t, restored.Decode(&legacy))
	require.Equal(t, txconfirm.BroadcastFailureUnknown, restored.Class)
}
