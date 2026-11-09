package lnruntime

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

// TestPeerChannelEventRPC verifies only peer-owned barrier facts survive the
// authenticated mailbox codec.
func TestPeerChannelEventRPC(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	expected := &arkchannel.RecoveryPackageInstalled{
		Party: arkchannel.PartyHub,
	}
	message, _, err := channelEventToRPC(
		id, expected,
	)
	require.NoError(t, err)
	decoded, err := channelEventFromRPC(message)
	require.NoError(t, err)
	require.Equal(t, expected, decoded)

	_, _, err = channelEventToRPC(
		id, &arkchannel.Materialize{},
	)
	require.ErrorContains(t, err, "unsupported remote channel event")
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
