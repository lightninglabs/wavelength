package virtualchannel

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnwire"
)

// PendingChannelLookup resolves lnd pending channel ids to persisted virtual
// channel negotiations.
type PendingChannelLookup interface {
	// FindVirtualChannelPendingOpen returns a virtual-channel intent by
	// lnd pending channel id and reports whether one exists.
	FindVirtualChannelPendingOpen(ctx context.Context,
		pendingID PendingChannelID) (*PendingOpen, bool, error)
}

// RegisteredChannelAcceptor accepts only registered virtual-channel zero-conf
// opens for integrated lnd runtimes.
type RegisteredChannelAcceptor struct {
	store PendingChannelLookup
	ctx   func() context.Context
}

// RegisteredChannelAcceptorConfig configures a store-backed acceptor.
type RegisteredChannelAcceptorConfig struct {
	Store   PendingChannelLookup
	Context func() context.Context
}

// NewRegisteredChannelAcceptor creates a store-backed virtual channel acceptor.
func NewRegisteredChannelAcceptor(cfg RegisteredChannelAcceptorConfig) (
	*RegisteredChannelAcceptor, error) {

	if cfg.Store == nil {
		return nil, fmt.Errorf("virtual channel acceptor store is nil")
	}
	if cfg.Context == nil {
		cfg.Context = context.Background
	}

	return &RegisteredChannelAcceptor{
		store: cfg.Store,
		ctx:   cfg.Context,
	}, nil
}

// Accept implements chanacceptor.ChannelAcceptor.
func (a *RegisteredChannelAcceptor) Accept(
	req *chanacceptor.ChannelAcceptRequest) *chanacceptor.ChannelAcceptResponse {

	if !wantsZeroConf(req) {
		return &chanacceptor.ChannelAcceptResponse{}
	}

	pending, ok, err := a.lookup(req)
	if err != nil {
		return rejectVirtualChannel(err)
	}
	if !ok {
		return rejectVirtualChannel(
			fmt.Errorf("virtual channel negotiation not " +
				"registered"),
		)
	}

	if err := validateRegisteredOpen(req, pending); err != nil {
		return rejectVirtualChannel(err)
	}

	return chanacceptor.NewChannelAcceptResponse(
		true, nil, nil, 0, 0, 0, 0, 0, 0, true,
	)
}

func (a *RegisteredChannelAcceptor) lookup(
	req *chanacceptor.ChannelAcceptRequest) (*PendingOpen, bool, error) {

	var pendingID PendingChannelID
	copy(pendingID[:], req.OpenChanMsg.PendingChannelID[:])

	return a.store.FindVirtualChannelPendingOpen(
		a.ctx(), pendingID,
	)
}

func validateRegisteredOpen(req *chanacceptor.ChannelAcceptRequest,
	pending *PendingOpen) error {

	if pending == nil {
		return fmt.Errorf("virtual channel pending open is nil")
	}
	if pending.Status != StatusNegotiating {
		return fmt.Errorf("virtual channel status %q is not "+
			"negotiating", pending.Status)
	}
	if len(pending.BackingVTXOs) == 0 {
		return fmt.Errorf("virtual channel has no backing VTXOs")
	}
	if req.Node == nil {
		return fmt.Errorf("virtual channel peer is nil")
	}

	remoteKey := req.Node.SerializeCompressed()
	if !bytes.Equal(remoteKey, pending.RemoteNodePubKey[:]) {
		return fmt.Errorf("virtual channel peer does not match " +
			"registered negotiation")
	}

	open := req.OpenChanMsg
	if open.FundingAmount != pending.Capacity {
		return fmt.Errorf("virtual channel capacity %d does not match "+
			"open amount %d", pending.Capacity, open.FundingAmount)
	}

	expectedPush := lnwire.MilliSatoshi(pending.LocalBalance * 1000)
	if open.PushAmount != expectedPush {
		return fmt.Errorf("virtual channel push amount %d does not "+
			"match local balance %d", open.PushAmount, expectedPush)
	}

	return nil
}

func rejectVirtualChannel(err error) *chanacceptor.ChannelAcceptResponse {
	return chanacceptor.NewChannelAcceptResponse(
		false, err, nil, 0, 0, 0, 0, 0, 0, false,
	)
}

// wantsZeroConf reports whether the peer requested a zero-conf channel type.
func wantsZeroConf(req *chanacceptor.ChannelAcceptRequest) bool {
	if req == nil || req.OpenChanMsg == nil ||
		req.OpenChanMsg.ChannelType == nil {
		return false
	}

	features := lnwire.RawFeatureVector(*req.OpenChanMsg.ChannelType)

	return features.IsSet(lnwire.ZeroConfRequired)
}

var _ chanacceptor.ChannelAcceptor = (*RegisteredChannelAcceptor)(nil)
