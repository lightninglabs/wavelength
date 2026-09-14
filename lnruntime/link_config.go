package lnruntime

import (
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
)

// LinkFailureHandler receives a terminal native channel-link failure.
type LinkFailureHandler func(lnwire.ChannelID, lnwire.ShortChannelID,
	htlcswitch.LinkFailureError)

// NewOnchainLinkConfig returns lnd's real chain event and contract callbacks.
// An unpublished channel point remains dormant until Ark materializes it.
func (r *Runtime) NewOnchainLinkConfig(peer lnpeer.Peer,
	channelPoint wire.OutPoint, onFailure LinkFailureHandler) (LinkConfig,
	error) {

	if r.onchain == nil {
		return LinkConfig{}, fmt.Errorf("on-chain lifecycle is " +
			"disabled")
	}
	if onFailure == nil {
		onFailure = func(lnwire.ChannelID, lnwire.ShortChannelID,
			htlcswitch.LinkFailureError) {
		}
	}
	events, updateSignals, notifyUpdate, err := r.onchain.LinkConfig(
		channelPoint,
	)
	if err != nil {
		return LinkConfig{}, err
	}

	return LinkConfig{
		Peer: peer,
		Policy: models.ForwardingPolicy{
			MinHTLCOut: 1, TimeLockDelta: 18,
		},
		ChainEvents:           events,
		SyncStates:            true,
		MaxAnchorFeeRate:      2_500,
		OnChannelFailure:      onFailure,
		UpdateContractSignals: updateSignals,
		NotifyContractUpdate:  notifyUpdate,
	}, nil
}
