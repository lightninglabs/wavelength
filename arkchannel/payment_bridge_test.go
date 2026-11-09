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

// TestPaymentBridgeRejectsFailureAfterPreimage proves a bridge never converts
// a known preimage into a failure path that could lose the source payment.
func TestPaymentBridgeRejectsFailureAfterPreimage(t *testing.T) {
	snapshot, preimage := testPaymentBridgeSnapshot(t, PaymentOutgoing)
	state, err := NewPaymentBridgeState(snapshot)
	require.NoError(t, err)

	state = applyPaymentEvent(t, state, &PaymentSourcePrepared{})
	state = applyPaymentEvent(t, state, &PaymentSourceHTLCLocked{})
	state = applyPaymentEvent(t, state, &PaymentDestinationStarted{
		ChannelID:       snapshot.ChannelID,
		DestinationSCID: snapshot.ReservedSCID,
	})
	require.Equal(t, snapshot.ReservedSCID, state.Snapshot().ReservedSCID)
	require.Equal(
		t, snapshot.ReservedSCID, state.Snapshot().DestinationSCID,
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
	snapshot, preimage := testPaymentBridgeSnapshot(t, PaymentOutgoing)
	advanced := snapshot.Clone()
	advanced.Phase = PaymentPreimageKnown
	advanced.DestinationSCID = 44
	advanced.Preimage = &preimage

	require.True(t, SamePaymentBridgeTerms(snapshot, advanced))
	advanced.SourceAmount++
	require.False(t, SamePaymentBridgeTerms(snapshot, advanced))
	advanced = snapshot.Clone()
	advanced.ChannelID = ID{3}
	require.False(t, SamePaymentBridgeTerms(snapshot, advanced))
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
	snapshot.ChannelID = ID{1}
	snapshot.PublicInvoice = "lnbcrt1test"

	return snapshot, preimage
}
