package lnruntime

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// changingFundingRPC models the operator's idempotency response cache while
// changing the authoritative channel phase after the first uncached read.
type changingFundingRPC struct {
	terms *arkchannelrpc.ChannelTerms

	nextKey uint64
	reads   uint64
	cached  map[string]*arkchannelrpc.GetFundingChannelResponse
	pending map[string]*arkchannelrpc.GetFundingChannelResponse
}

// newChangingFundingRPC constructs a mutable funding-status endpoint.
func newChangingFundingRPC(
	terms *arkchannelrpc.ChannelTerms) *changingFundingRPC {

	return &changingFundingRPC{
		terms: terms,
		cached: make(
			map[string]*arkchannelrpc.GetFundingChannelResponse,
		),
		pending: make(
			map[string]*arkchannelrpc.GetFundingChannelResponse,
		),
	}
}

// SendRPC returns the cached response for a reused idempotency key and reads
// the changing authoritative state only for a fresh logical request.
func (c *changingFundingRPC) SendRPC(_ context.Context,
	method mailboxrpc.ServiceMethod, _ proto.Message,
	options mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	if method.Method != "GetFundingChannel" {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected "+
			"method %s", method.Method)
	}
	key := options.IdempotencyKey
	if key == "" {
		c.nextKey++
		key = fmt.Sprintf("fresh-query-%d", c.nextKey)
	}
	response, ok := c.cached[key]
	if !ok {
		c.reads++
		phase := arkchannel.PhaseRequested
		if c.reads > 1 {
			phase = arkchannel.PhaseActive
		}
		response = &arkchannelrpc.GetFundingChannelResponse{
			Terms: c.terms,
			Phase: uint32(phase),
		}
		c.cached[key] = response
	}
	correlationID := options.CorrelationID
	if correlationID == "" {
		correlationID = key
	}
	c.pending[correlationID] = response

	return mailboxrpc.SendResult{
		CorrelationID:  correlationID,
		IdempotencyKey: key,
	}, nil
}

// AwaitRPC copies the response selected by SendRPC into the generated type.
func (c *changingFundingRPC) AwaitRPC(_ context.Context, correlationID string,
	response proto.Message) error {

	pending, ok := c.pending[correlationID]
	if !ok {
		return fmt.Errorf("response %q not found", correlationID)
	}
	delete(c.pending, correlationID)
	proto.Merge(response, pending)

	return nil
}

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

// TestGetFundingChannelUsesFreshRequestIdentity proves status polling cannot
// replay the first mutable channel snapshot from the operator's dedup cache.
func TestGetFundingChannelUsesFreshRequestIdentity(t *testing.T) {
	t.Parallel()

	terms, _, _, _ := statusPollChannel(t)
	transport := newChangingFundingRPC(channelTermsToRPC(terms))
	peer, err := NewMailboxFundingPeer(transport)
	require.NoError(t, err)

	first, err := peer.GetFundingChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseRequested, first.Phase)

	second, err := peer.GetFundingChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseActive, second.Phase)
	require.EqualValues(t, 2, transport.reads)
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

	cancelIncomingClient [33]byte
	cancelIncomingHash   lntypes.Hash
	cancelIncomingReason string
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
