package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

// clientArkChannelPeer binds the standalone engine to one hub. Only live
// lookups supply policy for new promotions; persisted terms belong to their
// original channel.
type clientArkChannelPeer struct {
	nodeKey              [33]byte
	materializationDelta uint32
}

// hubPromotionReady verifies the hub's durable phase for an explicit client
// promotion. Native link readiness alone does not make the channel selectable.
func (c *NativeArkChannelController) hubPromotionReady(ctx context.Context,
	s arkchannel.Snapshot) (bool, error) {

	peer := arkchannelrpc.NewArkChannelFundingPeerServiceMailboxClient(
		c.cfg.PeerRPC,
	)
	state, err := peer.GetFundingChannel(
		ctx, &arkchannelrpc.GetFundingChannelRequest{
			ChannelId: s.Terms.ID[:],
		},
	)
	if err != nil {
		return false, fmt.Errorf("read hub promotion: %w", err)
	}
	if !bytes.Equal(state.GetTerms().GetChannelId(), s.Terms.ID[:]) {
		return false, fmt.Errorf("hub returned a different channel")
	}

	switch state.GetPhase() {
	case uint32(arkchannel.PhaseActive):
		return true, nil

	case uint32(arkchannel.PhaseActivating):
		// A lost reply can interrupt the negotiator after it records
		// local activation. Observe the exact native funding point on
		// the hub; only a subsequent durable phase read is proof.
		if s.Backing == nil {
			return false, fmt.Errorf("active channel missing " +
				"backing")
		}
		_, err := c.fundingPeer.ChannelActive(ctx, s.Terms, *s.Backing)

		return false, err

	case uint32(arkchannel.PhaseRequested),
		uint32(arkchannel.PhaseNegotiating),
		uint32(arkchannel.PhaseBackingReady):
		return false, nil

	default:
		return false, fmt.Errorf("hub channel is not active: phase %d",
			state.GetPhase())
	}
}

// clientStartupPeer restores the peer that acknowledged these terms before
// local registration. A missing inventory is not proof of first use.
func (c *NativeArkChannelController) clientStartupPeer(ctx context.Context) (
	clientArkChannelPeer, error) {

	records, err := c.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return clientArkChannelPeer{}, err
	}
	var peer clientArkChannelPeer
	for _, record := range records {
		terms := record.Snapshot.Terms
		expected, err := c.newClientFundedTerms(
			terms.ID, terms.PendingChannelID, terms.ReservedSCID,
			terms.Capacity, fundingPeerFromTerms(terms),
		)
		if err != nil {
			return clientArkChannelPeer{}, err
		}
		if terms != expected {
			return clientArkChannelPeer{}, fmt.Errorf("channel %x "+
				"does not match the local wallet", terms.ID[:])
		}
		if peer.nodeKey != ([33]byte{}) &&
			peer.nodeKey != terms.HubNodeKey {
			return clientArkChannelPeer{}, fmt.Errorf("channel %x "+
				"belongs to a different hub", terms.ID[:])
		}
		peer.nodeKey = terms.HubNodeKey
	}
	if len(records) != 0 {
		peer.materializationDelta = ArkChannelMaterializationDelta(
			records, 0,
		)

		return peer, nil
	}

	info, err := liveFundingPeerInfo(ctx, c.remote)
	if err != nil {
		return clientArkChannelPeer{}, err
	}

	return clientArkChannelPeer{
		nodeKey:              info.HubNodeKey,
		materializationDelta: info.FunderDelay,
	}, nil
}

// fundingPeerFromTerms extracts only the hub roles from an immutable local
// promotion. Wallet roles are independently derived when rebuilding terms.
func fundingPeerFromTerms(t arkchannel.Terms) lnruntime.FundingPeerInfo {
	return lnruntime.FundingPeerInfo{
		HubNodeKey:       t.HubNodeKey,
		HubArkKey:        t.VTXO.HubArkKey,
		HubChannelKey:    t.VTXO.HubChannelKey,
		ArkOperatorKey:   t.VTXO.ArkOperatorKey,
		ChannelDelay:     t.VTXO.ChannelDelay,
		FunderDelay:      t.VTXO.FunderDelay,
		MinimumExitDelay: t.VTXO.MinExitDelay,
	}
}

// liveFundingPeerInfo obtains authenticated policy over the configured edge.
func liveFundingPeerInfo(ctx context.Context,
	remote lnruntime.ProcessFundingPeer) (lnruntime.FundingPeerInfo,
	error) {

	info, err := remote.GetPeerInfo(ctx)
	if err != nil {
		return lnruntime.FundingPeerInfo{}, fmt.Errorf("load Ark "+
			"channel hub policy: %w", err)
	}

	return info, info.Validate()
}

// promotionPolicy never substitutes historical terms for new funding or
// admits a policy that the running engine wasn't configured to enforce.
func (p clientArkChannelPeer) promotionPolicy(ctx context.Context,
	remote lnruntime.ProcessFundingPeer) (lnruntime.FundingPeerInfo,
	error) {

	info, err := liveFundingPeerInfo(ctx, remote)
	if err != nil {
		return lnruntime.FundingPeerInfo{}, err
	}
	if info.HubNodeKey != p.nodeKey ||
		info.FunderDelay != p.materializationDelta {
		return lnruntime.FundingPeerInfo{}, fmt.Errorf("hub policy " +
			"does not match the running channel endpoint")
	}

	return info, nil
}
