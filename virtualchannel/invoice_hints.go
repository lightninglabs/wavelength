package virtualchannel

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// ActiveChannelStore lists active virtual channels for invoice hop hints.
type ActiveChannelStore interface {
	// ListVirtualChannelsByStatus loads virtual channels by lifecycle
	// status.
	ListVirtualChannelsByStatus(context.Context, Status) ([]*Channel, error)
}

// InvoiceHopHintProvider contributes LND invoice hop hints for active virtual
// channels.
type InvoiceHopHintProvider struct {
	store ActiveChannelStore
}

// NewInvoiceHopHintProvider creates a virtual-channel invoice hint provider.
func NewInvoiceHopHintProvider(store ActiveChannelStore) (
	*InvoiceHopHintProvider, error) {

	if store == nil {
		return nil, fmt.Errorf("virtual channel hint store is nil")
	}

	return &InvoiceHopHintProvider{
		store: store,
	}, nil
}

// HopHints returns virtual-channel route hints for an invoice amount.
func (p *InvoiceHopHintProvider) HopHints(ctx context.Context,
	cfg *invoicesrpc.SelectHopHintsCfg, amtMSat lnwire.MilliSatoshi) (
	[][]zpay32.HopHint, error) {

	if cfg == nil {
		return nil, fmt.Errorf("hop hint config is nil")
	}

	channels, err := p.store.ListVirtualChannelsByStatus(
		ctx, StatusActive,
	)
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return nil, nil
	}

	openChannels, err := cfg.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	openByPoint := make(
		map[string]*channeldb.OpenChannel, len(openChannels),
	)
	for _, channel := range openChannels {
		openByPoint[channel.FundingOutpoint.String()] = channel
	}

	hints := make([][]zpay32.HopHint, 0, len(channels))
	for _, channel := range channels {
		if len(hints) >= cfg.MaxHopHints {
			break
		}

		openChannel := openByPoint[channel.ChannelPoint.String()]
		if openChannel == nil {
			continue
		}
		if !bytes.Equal(
			openChannel.IdentityPub.SerializeCompressed(),
			channel.RemoteNodePubKey[:],
		) {

			continue
		}

		hint, ok := virtualChannelHopHint(cfg, openChannel, amtMSat)
		if !ok {
			continue
		}

		hints = append(hints, []zpay32.HopHint{hint})
	}

	return hints, nil
}

func virtualChannelHopHint(cfg *invoicesrpc.SelectHopHintsCfg,
	channel *channeldb.OpenChannel,
	amtMSat lnwire.MilliSatoshi) (zpay32.HopHint, bool) {

	if channel.ChannelFlags&lnwire.FFAnnounceChannel != 0 {
		return zpay32.HopHint{}, false
	}

	chanID := lnwire.NewChanIDFromOutPoint(channel.FundingOutpoint)
	if !cfg.IsChannelActive(chanID) {
		return zpay32.HopHint{}, false
	}

	if amtMSat > 0 && channel.LocalCommitment.RemoteBalance < amtMSat {
		return zpay32.HopHint{}, false
	}

	scid := channel.ShortChannelID
	if channel.ChanType.HasScidAliasFeature() {
		alias, err := cfg.GetAlias(chanID)
		if err == nil && !alias.IsDefault() {
			scid = alias
		}
	}

	policy := virtualChannelRemotePolicy(cfg, channel)

	return zpay32.HopHint{
		NodeID:      channel.IdentityPub,
		ChannelID:   scid.ToUint64(),
		FeeBaseMSat: uint32(policy.FeeBaseMSat),
		FeeProportionalMillionths: uint32(
			policy.FeeProportionalMillionths,
		),
		CLTVExpiryDelta: policy.TimeLockDelta,
	}, true
}

func virtualChannelRemotePolicy(cfg *invoicesrpc.SelectHopHintsCfg,
	channel *channeldb.OpenChannel) *models.ChannelEdgePolicy {

	policy, ok := graphRemotePolicy(cfg, channel, channel.ShortChannelID)
	if ok {
		return policy
	}

	confirmedScid := channel.ZeroConfRealScid()
	policy, ok = graphRemotePolicy(cfg, channel, confirmedScid)
	if ok {
		return policy
	}

	return &models.ChannelEdgePolicy{
		TimeLockDelta:             chainreg.DefaultBitcoinTimeLockDelta,
		FeeBaseMSat:               chainreg.DefaultBitcoinBaseFeeMSat,
		FeeProportionalMillionths: 1,
	}
}

func graphRemotePolicy(cfg *invoicesrpc.SelectHopHintsCfg,
	channel *channeldb.OpenChannel,
	scid lnwire.ShortChannelID) (*models.ChannelEdgePolicy, bool) {

	if scid.IsDefault() {
		return nil, false
	}

	info, p1, p2, err := cfg.FetchChannelEdgesByID(scid.ToUint64())
	if err != nil {
		return nil, false
	}

	remotePub := channel.IdentityPub.SerializeCompressed()
	if bytes.Equal(remotePub, info.NodeKey1Bytes[:]) {
		return p1, p1 != nil
	}

	return p2, p2 != nil
}

var _ invoicesrpc.HopHintProvider = (*InvoiceHopHintProvider)(nil)
