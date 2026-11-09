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

// ChannelLinkRestoreError identifies one persisted channel that could not be
// restored without affecting its peer's other channels.
type ChannelLinkRestoreError struct {
	ChannelPoint wire.OutPoint
	Err          error
}

// Error returns the failed channel and its restore error.
func (e ChannelLinkRestoreError) Error() string {
	return fmt.Sprintf("restore lnd channel %v: %v", e.ChannelPoint, e.Err)
}

// Unwrap exposes the underlying per-channel failure.
func (e ChannelLinkRestoreError) Unwrap() error {
	return e.Err
}

// PeerLinkRestoreError aggregates independent channel restore failures.
type PeerLinkRestoreError struct {
	Failures []ChannelLinkRestoreError
}

// Error summarizes the partial peer restore.
func (e *PeerLinkRestoreError) Error() string {
	if e == nil {
		return "restore lnd peer channels"
	}

	return fmt.Sprintf("restore %d lnd peer channel(s)", len(e.Failures))
}

// Unwrap exposes each failed channel to errors.Is and errors.As.
func (e *PeerLinkRestoreError) Unwrap() []error {
	if e == nil {
		return nil
	}

	errs := make([]error, 0, len(e.Failures))
	for i := range e.Failures {
		errs = append(errs, e.Failures[i])
	}

	return errs
}

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
	failures := make([]ChannelLinkRestoreError, 0)

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
			failures = append(failures, ChannelLinkRestoreError{
				ChannelPoint: state.FundingOutpoint,
				Err: fmt.Errorf("inspect lnd channel link: %w",
					err),
			})

			continue
		}

		linkConfig, err := configSource(state)
		if err != nil {
			failures = append(failures, ChannelLinkRestoreError{
				ChannelPoint: state.FundingOutpoint,
				Err: fmt.Errorf("build lnd link "+
					"config: %w", err),
			})

			continue
		}
		linkConfig.Peer = peer
		linkConfig.SyncStates = true

		channel, err := r.AddLink(state, linkConfig)
		if err != nil {
			failures = append(failures, ChannelLinkRestoreError{
				ChannelPoint: state.FundingOutpoint,
				Err:          err,
			})

			continue
		}
		restored = append(restored, channel)
	}

	if len(failures) != 0 {
		return restored, &PeerLinkRestoreError{Failures: failures}
	}

	return restored, nil
}
