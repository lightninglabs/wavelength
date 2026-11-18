package lnruntime

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestPeerChannelEventRPC verifies only peer-owned barrier facts survive the
// authenticated mailbox codec.
func TestPeerChannelEventRPC(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	expected := &arkchannel.RecoveryPackageInstalled{
		Party: arkchannel.PartyClient,
	}
	message, _, err := channelEventToRPC(
		id, expected,
	)
	require.NoError(t, err)
	decoded, err := channelEventFromRPC(message)
	require.NoError(t, err)
	require.Equal(t, expected, decoded)

	message.Party = uint32(arkchannel.PartyHub)
	_, err = channelEventFromRPC(message)
	require.ErrorContains(t, err, "invalid recovery party")

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

// TestGetFundingChannelReadsDurablePromotion returns the source and revision
// used for lost-response reconciliation without reentering funding or signing.
func TestGetFundingChannelReadsDurablePromotion(t *testing.T) {
	t.Parallel()
	record, _ := testExplicitFundingRecord(t)
	server, backend, signer, recovery := newAuditFundingRPCServer(t, record)
	response, err := server.GetFundingChannel(
		t.Context(), &arkchannelrpc.GetFundingChannelRequest{
			ChannelId: record.Snapshot.Terms.ID[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, record.Revision, response.GetRevision())
	require.EqualValues(t, record.Snapshot.Phase, response.GetPhase())
	require.Equal(
		t, channelTermsToRPC(record.Snapshot.Terms),
		response.GetTerms(),
	)
	require.Equal(
		t, channelBindingToRPC(*record.Snapshot.Source),
		response.GetBinding(),
	)
	require.Nil(t, response.GetBacking())
	require.Zero(t, backend.calls.Load())
	require.Zero(t, signer.calls.Load())
	require.Zero(t, recovery.calls.Load())
}

// TestRegisterIncomingPaymentRequiresActiveChannel requires the bridge to
// select an existing channel before returning a registration.
func TestRegisterIncomingPaymentRequiresActiveChannel(t *testing.T) {
	bridge := &recordingPaymentBridgeCoordinator{}
	server := &FundingPeerRPCServer{
		cfg: FundingPeerRPCServerConfig{
			Bridge: bridge,
		},
	}
	hash := lntypes.Hash{1}
	req := &arkchannelrpc.RegisterIncomingPaymentRequest{
		PaymentHash: hash[:], AmountSat: 1234, ReservedScid: 5,
	}
	_, err := server.RegisterIncomingPayment(t.Context(), req)
	require.ErrorContains(t, err, "no active channel")
	bridge.channelID = arkchannel.ID{3}
	response, err := server.RegisterIncomingPayment(t.Context(), req)
	require.NoError(t, err)
	require.True(t, response.GetRegistered())
	require.Equal(t, bridge.channelID[:], response.GetChannelId())
}

// TestCancelIncomingPaymentRPCBindsClient proves pre-publication cleanup is
// applied to the bridge row owned by the authenticated mailbox endpoint.
func TestCancelIncomingPaymentRPCBindsClient(t *testing.T) {
	t.Parallel()

	client := [33]byte{2}
	hash := lntypes.Hash{3}
	bridge := &recordingPaymentBridgeCoordinator{}
	server := &FundingPeerRPCServer{cfg: FundingPeerRPCServerConfig{
		RemoteNode: client,
		Bridge:     bridge,
	}}
	response, err := server.CancelIncomingPayment(
		t.Context(), &arkchannelrpc.CancelIncomingPaymentRequest{
			PaymentHash: hash[:],
			Reason:      "invoice publication failed",
		},
	)
	require.NoError(t, err)
	require.True(t, response.GetCancelled())
	require.Equal(t, client, bridge.cancelIncomingClient)
	require.Equal(t, hash, bridge.cancelIncomingHash)
	require.Equal(
		t, "invoice publication failed", bridge.cancelIncomingReason,
	)
}

type recordingPaymentBridgeCoordinator struct {
	PaymentBridgeCoordinator

	channelID            arkchannel.ID
	cancelIncomingClient [33]byte
	cancelIncomingHash   lntypes.Hash
	cancelIncomingReason string
}

// RegisterIncomingPayment returns the selected channel identity in tests.
func (b *recordingPaymentBridgeCoordinator) RegisterIncomingPayment(
	context.Context, [33]byte, lntypes.Hash, btcutil.Amount, uint64) (
	arkchannel.ID, error) {

	return b.channelID, nil
}

// CancelIncomingPayment records authenticated receive cleanup.
func (b *recordingPaymentBridgeCoordinator) CancelIncomingPayment(
	_ context.Context, client [33]byte, hash lntypes.Hash,
	reason string) error {

	b.cancelIncomingClient = client
	b.cancelIncomingHash = hash
	b.cancelIncomingReason = reason

	return nil
}
