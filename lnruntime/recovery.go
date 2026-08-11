package lnruntime

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// LinkConfigSource supplies application-owned chain and materialization
// callbacks while restoring a channel from lnd's database.
type LinkConfigSource func(*chanstate.OpenChannel) (LinkConfig, error)

// RestorePeerLinks rebuilds every non-pending channel for one peer from lnd's
// database and enables the normal channel-reestablishment exchange. Existing
// links are left intact, making repeated recovery calls idempotent.
func (r *Runtime) RestorePeerLinks(peer lnpeer.Peer,
	configSource LinkConfigSource) ([]*lnwallet.LightningChannel, error) {

	if peer == nil {
		return nil, fmt.Errorf("channel peer is required")
	}
	if configSource == nil {
		return nil, fmt.Errorf("link config source is required")
	}

	states, err := r.cfg.DB.ChannelStateDB().FetchOpenChannels(
		peer.IdentityKey(),
	)
	if err != nil {
		return nil, fmt.Errorf("fetch lnd peer channels: %w", err)
	}

	restored := make([]*lnwallet.LightningChannel, 0, len(states))
	restoredPoints := make([]wire.OutPoint, 0, len(states))
	rollback := func() {
		for _, channelPoint := range restoredPoints {
			r.RemoveLink(channelPoint)
		}
	}

	for _, state := range states {
		if state.IsPending {
			continue
		}

		channelID := lnwire.NewChanIDFromOutPoint(
			state.FundingOutpoint,
		)
		if _, err := r.switcher.GetLink(channelID); err == nil {
			continue
		} else if !errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
			rollback()

			return nil, fmt.Errorf(
				"inspect lnd channel link: %w", err,
			)
		}

		linkConfig, err := configSource(state)
		if err != nil {
			rollback()

			return nil, fmt.Errorf("build lnd link config: %w", err)
		}
		linkConfig.Peer = peer
		linkConfig.SyncStates = true

		channel, err := r.AddLink(state, linkConfig)
		if err != nil {
			rollback()

			return nil, err
		}
		restored = append(restored, channel)
		restoredPoints = append(restoredPoints, state.FundingOutpoint)
	}

	return restored, nil
}
