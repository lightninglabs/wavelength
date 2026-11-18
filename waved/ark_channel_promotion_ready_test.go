package waved

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// promotionLookupStore exposes only lookup; any economic write fails the test.
type promotionLookupStore struct {
	arkchannel.Store
	record arkchannel.Record
}

// Get returns the existing channel only for the original promotion identity.
func (s *promotionLookupStore) Get(ctx context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	if id != s.record.Snapshot.Terms.ID {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}

	return s.record, ctx.Err()
}

type promotionStatusRPC struct {
	t       *testing.T
	id      arkchannel.ID
	phase   uint32
	reads   int
	probes  int
	readErr error
	observe func() error
}

// SendRPC allows only status requests for the expected funding point.
func (p *promotionStatusRPC) SendRPC(ctx context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	switch msg := req.(type) {
	case *arkchannelrpc.GetFundingChannelRequest:
		require.Equal(p.t, "GetFundingChannel", method.Method)
		require.Equal(p.t, p.id[:], msg.GetChannelId())
		p.reads++

	case *arkchannelrpc.FundingStatusRequest:
		require.Equal(p.t, "ChannelActive", method.Method)
		require.Equal(p.t, p.id[:], msg.GetTerms().GetChannelId())
		require.EqualValues(
			p.t, 17, msg.GetBacking().GetChannelPointIndex(),
		)
		p.probes++

	default:
		p.t.Fatalf("unexpected promotion side effect: %T", msg)
	}

	return mailboxrpc.SendResult{CorrelationID: method.Method}, ctx.Err()
}

// AwaitRPC distinguishes native readiness from the hub's durable phase.
func (p *promotionStatusRPC) AwaitRPC(ctx context.Context, _ string,
	response proto.Message) error {

	switch msg := response.(type) {
	case *arkchannelrpc.GetFundingChannelResponse:
		msg.Terms = &arkchannelrpc.ChannelTerms{ChannelId: p.id[:]}
		msg.Phase = p.phase

		return p.readErr

	case *arkchannelrpc.FundingStatusResponse:
		msg.Ready = true

		return p.observe()

	default:
		p.t.Fatalf("unexpected promotion reply: %T", msg)

		return ctx.Err()
	}
}

// TestPromotionWaitsForHubActivation exercises explicit promotion replay
// without a funding peer, payment bridge or writable store.
func TestPromotionWaitsForHubActivation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, testPromotionWaitsForHubActivation)
}

// testPromotionWaitsForHubActivation uses virtual time for the polling path.
func testPromotionWaitsForHubActivation(t *testing.T) {
	c := &NativeArkChannelController{
		party: arkchannel.PartyClient,
		cfg: ArkChannelControllerConfig{
			IdentityKey: testKeyDescriptor(t, 1),
		},
		keys: nativeArkChannelKeys{
			ark:     testKeyDescriptor(t, 2),
			backing: testKeyDescriptor(t, 3),
			funder:  testKeyDescriptor(t, 4),
		},
		node: &lnruntime.NativeNode{},
	}
	const key = "replay"
	id, pending, scid := c.promotionIdentifiers(key)
	info := fundingPeerFromTerms(testPrePONRTerms(t))
	terms, err := c.newClientFundedTerms(id, pending, scid, 200_000, info)
	require.NoError(t, err)
	c.clientPeer.nodeKey = terms.HubNodeKey
	store := &promotionLookupStore{record: arkchannel.Record{
		Snapshot: arkchannel.Snapshot{
			Phase: arkchannel.PhaseActive, Terms: terms,
			Source:  &arkchannel.VTXOBinding{},
			Backing: &arkchannel.Backing{},
		},
	}}
	store.record.Snapshot.Backing.ChannelPoint.Index = 17
	c.coordinator, err = arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	rejectAction := arkChannelResumeExecutor(func(_ context.Context,
		_ arkchannel.ID, action arkchannel.Action) error {

		t.Fatalf("unexpected channel action on replay: %T", action)

		return nil
	})
	c.service, err = arkchannel.NewService(
		c.party, c.coordinator, rejectAction,
	)
	require.NoError(t, err)
	p := &promotionStatusRPC{
		t: t, id: id, phase: uint32(arkchannel.PhaseActivating),
	}
	c.cfg.PeerRPC = p
	c.fundingPeer, err = lnruntime.NewMailboxFundingPeer(p)
	require.NoError(t, err)
	promote := func(ctx context.Context) (arkchannel.Record, error) {
		return c.PromoteVTXO(ctx, terms.Capacity, key)
	}

	offline := errors.New("peer unavailable")
	p.readErr = offline
	record, err := promote(t.Context())
	require.ErrorIs(t, err, offline)
	require.Zero(t, record)
	require.Zero(t, p.probes)

	p.readErr = nil
	p.observe = func() error { return offline }
	_, err = promote(t.Context())
	require.ErrorIs(t, err, offline)
	require.Equal(t, 1, p.probes)

	// Even a positive native observation is not a durable phase read.
	// Leave the hub activating and cancel before the next poll.
	ctx, cancel := context.WithCancel(t.Context())
	p.observe = func() error {
		cancel()

		return nil
	}
	_, err = promote(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, p.probes)

	before := p.reads
	p.observe = func() error {
		p.phase = uint32(arkchannel.PhaseActive)

		return nil
	}
	record, err = promote(t.Context())
	require.NoError(t, err)
	require.Equal(t, store.record, record)
	require.Equal(t, before+2, p.reads)
	require.Equal(t, 3, p.probes)

	// The hub can persist activation even if its reply is lost.
	p.phase = uint32(arkchannel.PhaseActivating)
	p.observe = func() error {
		p.phase = uint32(arkchannel.PhaseActive)

		return offline
	}
	_, err = promote(t.Context())
	require.ErrorIs(t, err, offline)
	before = p.reads
	record, err = promote(t.Context())
	require.NoError(t, err)
	require.Equal(t, store.record, record)
	require.Equal(t, before+1, p.reads)
	require.Equal(t, 4, p.probes)

	// A hub already closing cannot authorize a stale active response.
	p.phase = uint32(arkchannel.PhaseCoopClosing)
	_, err = promote(t.Context())
	require.ErrorContains(t, err, "hub channel is not active")
	require.Equal(t, 4, p.probes)

	// Local post-creation lookups stay usable with the hub offline.
	before = p.reads
	p.readErr = offline
	store.record.Snapshot.Phase = arkchannel.PhaseClosed
	record, err = promote(t.Context())
	require.NoError(t, err)
	require.Equal(t, store.record, record)
	require.Equal(t, before, p.reads)
}
