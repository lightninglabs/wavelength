package lnruntime

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

// TestMaterializationChannelEventRPC verifies the durable handoff and backing
// publication facts survive the authenticated mailbox codec exactly.
func TestMaterializationChannelEventRPC(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	materialize, _, err := channelEventToRPC(
		id, &arkchannel.Materialize{},
	)
	require.NoError(t, err)
	decoded, err := channelEventFromRPC(materialize)
	require.NoError(t, err)
	require.IsType(t, &arkchannel.Materialize{}, decoded)

	txID := chainhash.Hash{9, 8, 7}
	published, _, err := channelEventToRPC(
		id, &arkchannel.BackingPublished{
			TxID: txID,
		},
	)
	require.NoError(t, err)
	decoded, err = channelEventFromRPC(published)
	require.NoError(t, err)
	publishedEvent, ok := decoded.(*arkchannel.BackingPublished)
	require.True(t, ok)
	require.Equal(t, txID, publishedEvent.TxID)
}

// TestOORAbortedChannelEventRPC proves peer cancellation carries the exact
// prepared session and stable failure reason.
func TestOORAbortedChannelEventRPC(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	expected := &arkchannel.OORAborted{
		SessionID: [32]byte{
			9,
			8,
			7,
		},
		Reason: "pre-PONR channel negotiation expired",
	}
	message, _, err := channelEventToRPC(id, expected)
	require.NoError(t, err)
	decoded, err := channelEventFromRPC(message)
	require.NoError(t, err)
	actual, ok := decoded.(*arkchannel.OORAborted)
	require.True(t, ok)
	require.Equal(t, expected, actual)
}
