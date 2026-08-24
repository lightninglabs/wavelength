package lnruntime

import (
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
)

// CooperativeCloseRequest contains the application-owned edges around lnd's
// native cooperative-close state machine. Ark materialization must complete
// before the BroadcastTx callback publishes the resulting closing transaction.
type CooperativeCloseRequest struct {
	ChannelPoint      wire.OutPoint
	DeliveryAddress   chancloser.DeliveryAddrWithKey
	IdealFeeRate      chainfee.SatPerKWeight
	MaxFeeRate        chainfee.SatPerKWeight
	NegotiationHeight uint32
	Closer            lntypes.ChannelParty
	MusigSession      chancloser.MusigSession

	BroadcastTx    func(*wire.MsgTx, string) error
	DisableChannel func(wire.OutPoint) error
	Disconnect     func() error
}

// NewCooperativeClose reconstructs a channel from lnd's database and returns
// lnd's native cooperative-close state machine. The caller transports the
// resulting Shutdown and ClosingSigned messages to the counterparty.
func (r *Runtime) NewCooperativeClose(req CooperativeCloseRequest) (
	*chancloser.ChanCloser, error) {

	if req.ChannelPoint == (wire.OutPoint{}) {
		return nil, fmt.Errorf("channel point is required")
	}
	if len(req.DeliveryAddress.DeliveryAddress) == 0 {
		return nil, fmt.Errorf("cooperative close delivery address " +
			"is required")
	}
	if req.IdealFeeRate <= 0 {
		return nil, fmt.Errorf("cooperative close fee rate is required")
	}
	if req.Closer != lntypes.Local && req.Closer != lntypes.Remote {
		return nil, fmt.Errorf("cooperative close party is invalid")
	}
	if req.BroadcastTx == nil {
		return nil, fmt.Errorf("cooperative close broadcaster is " +
			"required")
	}
	if r.funding == nil || r.funding.netParams == nil {
		return nil, fmt.Errorf("cooperative close requires funding " +
			"runtime")
	}

	channelState, err := r.cfg.DB.ChannelStateDB().FetchChannel(
		req.ChannelPoint,
	)
	if err != nil {
		return nil, fmt.Errorf("find channel for cooperative close: %w",
			err)
	}
	if channelState.IsPending {
		return nil, fmt.Errorf("cannot cooperatively close a pending " +
			"channel")
	}
	if channelState.ChanType.IsTaproot() && req.MusigSession == nil {
		return nil, fmt.Errorf("taproot cooperative close requires a " +
			"MuSig2 session")
	}

	channel, err := lnwallet.NewLightningChannel(
		r.cfg.Signer, channelState, r.sigPool,
	)
	if err != nil {
		return nil, fmt.Errorf("restore channel for cooperative "+
			"close: %w", err)
	}

	disableChannel := req.DisableChannel
	if disableChannel == nil {
		disableChannel = func(channelPoint wire.OutPoint) error {
			r.RemoveLink(channelPoint)

			return nil
		}
	}
	disconnect := req.Disconnect
	if disconnect == nil {
		disconnect = func() error { return nil }
	}
	maxFeeRate := req.MaxFeeRate
	if maxFeeRate == 0 {
		maxFeeRate = req.IdealFeeRate
	}
	if maxFeeRate < req.IdealFeeRate {
		return nil, fmt.Errorf("maximum cooperative close fee rate %v "+
			"is below ideal rate %v", maxFeeRate, req.IdealFeeRate)
	}

	return chancloser.NewChanCloser(
		chancloser.ChanCloseCfg{
			Channel:        channel,
			MusigSession:   req.MusigSession,
			BroadcastTx:    req.BroadcastTx,
			DisableChannel: disableChannel,
			Disconnect:     disconnect,
			MaxFee:         maxFeeRate,
			ChainParams:    r.funding.netParams,
			Quit:           make(chan struct{}),
			FeeEstimator:   &chancloser.SimpleCoopFeeEstimator{},
		}, req.DeliveryAddress, req.IdealFeeRate,
		req.NegotiationHeight, nil, req.Closer,
	), nil
}
