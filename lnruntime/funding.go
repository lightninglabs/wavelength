package lnruntime

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	nativeChannelCSVDelay = lndfunding.MinBtcRemoteDelay
	nativeChannelMaxHTLCs = uint16(input.MaxHTLCNumber / 2)
	nativeChannelMinHTLC  = lnwire.MilliSatoshi(1)
)

// FundingOpenRequest starts an externally funded private channel with an
// explicit initial push allocation.
type FundingOpenRequest struct {
	Peer             lnpeer.Peer
	PendingChannelID lndfunding.PendingChanID
	Capacity         btcutil.Amount
	PushAmount       btcutil.Amount
	BasePSBT         *psbt.Packet
}

// FundingFlow exposes lnd's native asynchronous funding progress.
type FundingFlow struct {
	PendingChannelID lndfunding.PendingChanID
	Updates          <-chan *lnrpc.OpenStatusUpdate
	Errors           <-chan error
}

// defaultRemoteReserve preserves lnd's standard one-percent remote reserve.
func defaultRemoteReserve(capacity, dustLimit btcutil.Amount) btcutil.Amount {
	reserve := capacity / 100
	if reserve < dustLimit {
		return dustLimit
	}

	return reserve
}

// defaultRemoteMaxValue allows all remote liquidity except the exact reserve.
func defaultRemoteMaxValue(capacity btcutil.Amount) lnwire.MilliSatoshi {
	reserve := defaultRemoteReserve(
		capacity, lnwallet.DustLimitUnknownWitness(),
	)

	return lnwire.NewMSatFromSatoshis(capacity - reserve)
}

// nativeChannelType returns the only commitment type admitted by this runtime.
func nativeChannelType() *lnwire.ChannelType {
	return (*lnwire.ChannelType)(lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	),
	)
}

// isNativeChannelType reports whether an explicit wire type is exactly the
// static-remote-key zero-fee-anchor commitment type.
func isNativeChannelType(channelType *lnwire.ChannelType) bool {
	if channelType == nil {
		return false
	}

	return (*lnwire.RawFeatureVector)(channelType).Equals(
		(*lnwire.RawFeatureVector)(nativeChannelType()),
	)
}
