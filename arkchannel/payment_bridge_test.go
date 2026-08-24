package arkchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestPaymentBridgeOutgoing proves the public preimage is durable before the
// private source can settle.
func TestPaymentBridgeOutgoing(t *testing.T) {
	snapshot, preimage := testPaymentBridgeSnapshot(t, PaymentOutgoing)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)

	state = applyPaymentEvent(t, state, &PaymentSourcePrepared{})
	require.Equal(t, PaymentSourceReady, state.Snapshot().Phase)
	state = applyPaymentEvent(t, state, &PaymentSourceHTLCLocked{})
	require.Equal(t, PaymentSourceLocked, state.Snapshot().Phase)
	state = applyPaymentEvent(t, state, &PaymentDestinationStarted{
		ChannelID:       snapshot.ChannelID,
		DestinationSCID: snapshot.ReservedSCID,
	})
	require.Equal(t, PaymentDestinationInFlight, state.Snapshot().Phase)
	state = applyPaymentEvent(t, state, &PaymentDestinationSettled{
		Preimage: preimage,
	})
	require.Equal(t, PaymentPreimageKnown, state.Snapshot().Phase)
	require.Equal(t, preimage, *state.Snapshot().Preimage)
	state = applyPaymentEvent(t, state, &PaymentSourceSettled{})
	require.True(t, state.IsTerminal())
	require.Equal(t, PaymentCompleted, state.Snapshot().Phase)
}

// TestPaymentBridgeIncomingFallback proves insufficient private liquidity can
// only hand a pristine incoming reservation to the existing vHTLC lifecycle.
func TestPaymentBridgeIncomingFallback(t *testing.T) {
	snapshot, _ := testPaymentBridgeSnapshot(t, PaymentIncoming)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)

	state = applyPaymentEvent(t, state, &PaymentFallbackSelected{
		Reason: "insufficient private inbound liquidity",
	})
	require.True(t, state.IsTerminal())
	require.Equal(t, PaymentVHTLCFallback, state.Snapshot().Phase)

	_, err = state.ProcessEvent(
		t.Context(), &PaymentSourceHTLCLocked{Circuit: &PaymentCircuit{
			IncomingChannelID: 1, IncomingHTLCID: 2,
		}}, &PaymentBridgeEnvironment{
			PaymentHash: snapshot.PaymentHash,
		},
	)
	require.Error(t, err)
}

// TestPaymentBridgeRejectsFailureAfterPreimage proves a bridge never converts
// a known preimage into a failure path that could lose the source payment.
func TestPaymentBridgeRejectsFailureAfterPreimage(t *testing.T) {
	snapshot, preimage := testPaymentBridgeSnapshot(t, PaymentIncoming)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)

	state = applyPaymentEvent(t, state, &PaymentSourceHTLCLocked{
		Circuit: &PaymentCircuit{
			IncomingChannelID: 1, IncomingHTLCID: 2,
			OutgoingSCID: snapshot.ReservedSCID,
		},
	})
	state = applyPaymentEvent(t, state, &PaymentDestinationStarted{
		ChannelID:       ID{2},
		DestinationSCID: snapshot.ReservedSCID + 1,
	})
	require.Equal(t, snapshot.ReservedSCID, state.Snapshot().ReservedSCID)
	require.Equal(
		t, snapshot.ReservedSCID+1, state.Snapshot().DestinationSCID,
	)
	state = applyPaymentEvent(t, state, &PaymentDestinationSettled{
		Preimage: preimage,
	})

	_, err = state.ProcessEvent(
		t.Context(), &PaymentDestinationFailed{
			Reason: "late failure",
		}, &PaymentBridgeEnvironment{
			PaymentHash: snapshot.PaymentHash,
		},
	)
	require.ErrorContains(t, err, "cannot fail after preimage")
}

// TestPaymentBridgeOutgoingSourceUnavailable proves client-side failure before
// the hold invoice locks cannot dispatch the public destination payment.
func TestPaymentBridgeOutgoingSourceUnavailable(t *testing.T) {
	snapshot, _ := testPaymentBridgeSnapshot(t, PaymentOutgoing)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)
	state = applyPaymentEvent(t, state, &PaymentSourcePrepared{})
	state = applyPaymentEvent(t, state, &PaymentSourceUnavailable{
		Reason: "private route failed",
	})

	require.Equal(t, PaymentSourceFailing, state.Snapshot().Phase)
	_, ok := PendingPaymentBridgeAction(
		state.Snapshot(),
	).(*FailPaymentSource)
	require.True(t, ok)

	_, err = state.ProcessEvent(
		t.Context(), &PaymentDestinationStarted{
			ChannelID:       snapshot.ChannelID,
			DestinationSCID: snapshot.ReservedSCID,
		}, &PaymentBridgeEnvironment{PaymentHash: snapshot.PaymentHash},
	)
	require.Error(t, err)
}

// TestSamePaymentBridgeTerms excludes worker-owned lifecycle fields while
// retaining all caller-owned reservation terms.
func TestSamePaymentBridgeTerms(t *testing.T) {
	snapshot, preimage := testPaymentBridgeSnapshot(t, PaymentIncoming)
	advanced := snapshot.Clone()
	advanced.Phase = PaymentPreimageKnown
	advanced.ChannelID = ID{3}
	advanced.DestinationSCID = 44
	advanced.Circuit = &PaymentCircuit{IncomingChannelID: 1}
	advanced.Preimage = &preimage

	require.True(t, SamePaymentBridgeTerms(snapshot, advanced))
	advanced.SourceAmount++
	require.False(t, SamePaymentBridgeTerms(snapshot, advanced))
}

// TestPaymentBridgeIncomingDispatchFallback proves a private liquidity race
// can return to the vHTLC rail until either destination reveals a preimage.
func TestPaymentBridgeIncomingDispatchFallback(t *testing.T) {
	snapshot, _ := testPaymentBridgeSnapshot(t, PaymentIncoming)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)

	state = applyPaymentEvent(t, state, &PaymentSourceHTLCLocked{
		Circuit: &PaymentCircuit{
			IncomingChannelID: 1, IncomingHTLCID: 2,
			OutgoingSCID: snapshot.ReservedSCID,
		},
	})
	state = applyPaymentEvent(t, state, &PaymentDestinationStarted{
		ChannelID: ID{2}, DestinationSCID: 43,
	})
	state = applyPaymentEvent(t, state, &PaymentFallbackSelected{
		Reason: "private channel liquidity changed",
	})

	require.Equal(t, PaymentVHTLCFallback, state.Snapshot().Phase)
	require.Equal(t, snapshot.ReservedSCID, state.Snapshot().ReservedSCID)
}

func applyPaymentEvent(t *testing.T, state PaymentBridgeState,
	event PaymentBridgeEvent) PaymentBridgeState {

	t.Helper()
	transition, err := state.ProcessEvent(
		t.Context(), event, &PaymentBridgeEnvironment{
			PaymentHash: state.Snapshot().PaymentHash,
		},
	)
	require.NoError(t, err)
	next, ok := transition.NextState.(PaymentBridgeState)
	require.True(t, ok)

	return next
}

func testPaymentBridgeSnapshot(t *testing.T,
	direction PaymentDirection) (PaymentBridgeSnapshot, lntypes.Preimage) {

	t.Helper()
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage := lntypes.Preimage{1, 2, 3}
	var clientNode [33]byte
	copy(clientNode[:], clientKey.PubKey().SerializeCompressed())
	snapshot := PaymentBridgeSnapshot{
		Direction: direction, PaymentHash: preimage.Hash(),
		ClientNodeKey: clientNode, ReservedSCID: 42,
		SourceAmount:      btcutil.Amount(2_100),
		DestinationAmount: btcutil.Amount(2_000),
		ServerFee:         btcutil.Amount(60),
		RoutingFeeBudget:  btcutil.Amount(40),
	}
	if direction == PaymentOutgoing {
		snapshot.ChannelID = ID{1}
		snapshot.PublicInvoice = "lnbcrt1test"
	} else {
		snapshot.SourceAmount = snapshot.DestinationAmount
		snapshot.ServerFee = 0
		snapshot.RoutingFeeBudget = 0
	}

	return snapshot, preimage
}
