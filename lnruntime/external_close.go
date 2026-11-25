package lnruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CleanChannelState is the settled zero-fee allocation at one clean lnd
// commitment height. Balances are relative to the runtime returning it.
type CleanChannelState struct {
	ChannelPoint     wire.OutPoint
	LocalBalance     btcutil.Amount
	RemoteBalance    btcutil.Amount
	Capacity         btcutil.Amount
	CommitmentHeight uint64
	LocalInitiator   bool
}

// QuiesceChannel prevents new HTLC adds in either direction and waits until
// the native lnd channel has no HTLCs, pending commitments, or unsigned
// updates. Existing settles and failures continue while the link drains.
func (r *Runtime) QuiesceChannel(ctx context.Context,
	channelPoint wire.OutPoint) (CleanChannelState, error) {

	return r.quiesceChannel(ctx, channelPoint, false)
}

// quiesceChannel distinguishes a caller-owned temporary flush from the
// durable external-close hold. Only a recorded unsigned abort releases the
// latter, even if this drain fails or the signing response was lost.
func (r *Runtime) quiesceChannel(ctx context.Context,
	channelPoint wire.OutPoint, durable bool) (CleanChannelState, error) {

	if err := ctx.Err(); err != nil {
		return CleanChannelState{}, err
	}
	channelID := lnwire.NewChanIDFromOutPoint(channelPoint)
	r.linkMu.Lock()
	link, err := r.switcher.GetLink(channelID)
	changed := r.linkChanges
	var restoreIncoming, restoreOutgoing bool
	var managerDone <-chan struct{}
	if err == nil {
		if durable {
			managerDone, err = setExternalCloseHold(link, true)
		} else {
			restoreIncoming = link.DisableAdds(htlcswitch.Incoming)
			restoreOutgoing = link.DisableAdds(htlcswitch.Outgoing)
		}
	}
	r.linkMu.Unlock()
	resume := true
	defer func() {
		// Roll back only directions this temporary call acquired on
		// this link. Neither operation can clear an external hold.
		if resume && restoreIncoming {
			link.EnableAdds(htlcswitch.Incoming)
		}
		if resume && restoreOutgoing {
			link.EnableAdds(htlcswitch.Outgoing)
		}
	}()
	switch {
	case err == nil:
		flushed := make(chan struct{})
		err = link.OnFlushedOnce(ctx, func() { close(flushed) })
		if err != nil &&
			!errors.Is(err, htlcswitch.ErrLinkShuttingDown) {
			return CleanChannelState{}, err
		}
		// Registration can return ErrLinkShuttingDown before the
		// manager finishes its current update. Only its actual exit
		// substitutes for a clean callback on a durable close.
		if err == nil || managerDone != nil {
		drain:
			for {
				select {
				case <-flushed:
					break drain

				case <-managerDone:
					break drain

				case <-ctx.Done():
					return CleanChannelState{}, ctx.Err()

				case <-r.quit:
					return CleanChannelState{},
						htlcswitch.ErrLinkShuttingDown

				case <-changed:
					r.linkMu.Lock()
					current, linkErr := r.switcher.GetLink(
						channelID,
					)
					changed = r.linkChanges
					r.linkMu.Unlock()
					if linkErr == nil && current == link {
						continue
					}

					return CleanChannelState{},
						htlcswitch.ErrLinkShuttingDown

				}
			}
		}

	case errors.Is(err, htlcswitch.ErrChannelLinkNotFound):
		// A restart may replay the close before links are restored.
		// With no live link, the persisted channel cannot accept new
		// updates and is safe to inspect directly.

	default:
		return CleanChannelState{}, fmt.Errorf("find channel link: %w",
			err)
	}

	r.linkMu.Lock()
	defer r.linkMu.Unlock()
	current, currentErr := r.switcher.GetLink(channelID)
	// Switch shutdown deindexes links before it joins their managers.
	// In that interval a missing link is not evidence of an offline DB.
	select {
	case <-r.quit:
		return CleanChannelState{}, htlcswitch.ErrLinkShuttingDown

	default:
	}
	if current != link || (currentErr != nil && !errors.Is(
		currentErr, htlcswitch.ErrChannelLinkNotFound,
	)) {
		return CleanChannelState{}, htlcswitch.ErrLinkShuttingDown
	}
	if link != nil && (!link.IsFlushing(htlcswitch.Incoming) ||
		!link.IsFlushing(htlcswitch.Outgoing)) {
		return CleanChannelState{}, fmt.Errorf("channel drain was " +
			"resumed")
	}
	state, err := r.cleanChannelState(channelPoint)
	resume = err != nil

	return state, err
}

// ResumeChannel releases a temporary flush but cannot release an external
// settlement's independent hold.
func (r *Runtime) ResumeChannel(channelPoint wire.OutPoint) {
	r.resumeChannelLink(channelPoint)
}

// setExternalCloseHold fails closed on links without the external commitment
// barrier. Ordinary forwarding and directional flushes need no fork extension.
func setExternalCloseHold(link htlcswitch.ChannelUpdateHandler,
	hold bool) (<-chan struct{}, error) {

	controller, ok := link.(interface {
		SetExternalCloseHold(bool) <-chan struct{}
	})
	if !ok {
		if !hold {

			// An older link could never acquire an external hold.
			return nil, nil
		}

		return nil, errors.New("link does not support external close " +
			"hold")
	}
	done := controller.SetExternalCloseHold(hold)
	if done == nil {
		return nil, errors.New("external close has no link exit signal")
	}

	return done, nil
}

// resumeChannelLink restores adds when the channel link still exists.
func (r *Runtime) resumeChannelLink(channelPoint wire.OutPoint) {
	link, err := r.switcher.GetLink(
		lnwire.NewChanIDFromOutPoint(channelPoint),
	)
	if err != nil {
		return
	}
	link.EnableAdds(htlcswitch.Incoming)
	link.EnableAdds(htlcswitch.Outgoing)
}

// cleanChannelState returns the exact zero-fee cooperative balances once both
// commitment chains are synchronized.
func (r *Runtime) cleanChannelState(channelPoint wire.OutPoint) (
	CleanChannelState, error) {

	state, err := r.cfg.DB.ChannelStateDB().FetchChannel(channelPoint)
	if err != nil {
		return CleanChannelState{}, fmt.Errorf("find channel for "+
			"cooperative close: %w", err)
	}
	if state.IsPending {
		return CleanChannelState{}, fmt.Errorf("cannot cooperatively " +
			"close a pending channel")
	}
	channel, err := lnwallet.NewLightningChannel(
		r.cfg.Signer, state, r.sigPool,
	)
	if err != nil {
		return CleanChannelState{}, fmt.Errorf("restore channel for "+
			"cooperative close: %w", err)
	}
	if !channel.IsChannelClean() {
		return CleanChannelState{}, fmt.Errorf("channel commitments " +
			"are not clean")
	}
	if state.LocalCommitment.CommitHeight !=
		state.RemoteCommitment.CommitHeight {
		return CleanChannelState{}, fmt.Errorf("channel commitment " +
			"heights are not synchronized")
	}

	snapshot := channel.StateSnapshot()
	localBalance, remoteBalance, err := externalCloseBalances(
		state.ChanType, state.IsInitiator, snapshot,
	)
	if err != nil {
		return CleanChannelState{}, fmt.Errorf("derive cooperative "+
			"close balances: %w", err)
	}

	return CleanChannelState{
		ChannelPoint:     channelPoint,
		LocalBalance:     localBalance,
		RemoteBalance:    remoteBalance,
		Capacity:         snapshot.Capacity,
		CommitmentHeight: snapshot.CommitHeight,
		LocalInitiator:   state.IsInitiator,
	}, nil
}

// externalCloseBalances returns lnd's zero-fee balances, including the
// commitment fee and anchors. Complementary fractions form one satoshi;
// both endpoints credit it to the funding initiator, never to "local".
func externalCloseBalances(chanType channeldb.ChannelType, initiator bool,
	s *channeldb.ChannelSnapshot) (btcutil.Amount, btcutil.Amount, error) {

	unit := lnwire.NewMSatFromSatoshis(1)
	remainder := s.LocalBalance%unit + s.RemoteBalance%unit
	local := s.LocalBalance.ToSatoshis()
	remote := s.RemoteBalance.ToSatoshis()
	switch remainder {
	case 0:
	case unit:
		if initiator {
			local++
		} else {
			remote++
		}

	default:
		return 0, 0, fmt.Errorf("fractional external close total: %v "+
			"remainder", remainder)
	}

	local, remote, err := lnwallet.CoopCloseBalance(
		chanType, initiator, 0, local, remote, s.CommitFee,
		fn.None[lntypes.ChannelParty](),
	)
	if err != nil {
		return 0, 0, err
	}
	if local > s.Capacity || remote != s.Capacity-local {
		return 0, 0, fmt.Errorf("cooperative balances %d + %d do not "+
			"match capacity %d", local, remote, s.Capacity)
	}

	return local, remote, nil
}

// FinalizeExternalCooperativeClose archives a channel whose clean balance was
// settled by an application-owned transaction. The close is idempotent across
// a crash after the open-channel record was removed.
func (r *Runtime) FinalizeExternalCooperativeClose(channelPoint wire.OutPoint,
	closingTxID chainhash.Hash, settledBalance btcutil.Amount,
	localInitiated bool) error {

	stateDB := r.cfg.DB.ChannelStateDB()
	channel, err := stateDB.FetchChannel(channelPoint)
	if errors.Is(err, channeldb.ErrChannelNotFound) {
		closed, closedErr := stateDB.FetchClosedChannel(&channelPoint)
		if closedErr != nil {
			return fmt.Errorf("find externally closed channel: %w",
				closedErr)
		}
		if closed.CloseType != channeldb.CooperativeClose ||
			closed.ClosingTXID != closingTxID ||
			closed.SettledBalance != settledBalance {
			return fmt.Errorf("channel was archived with another " +
				"close")
		}

		return r.forgetExternallyClosedChannel(channelPoint)
	}
	if err != nil {
		return fmt.Errorf("find channel for external close: %w", err)
	}
	if channel.IsPending {
		return fmt.Errorf("cannot archive a pending channel")
	}
	if settledBalance < 0 || settledBalance > channel.Capacity {
		return fmt.Errorf("settled balance %d exceeds channel "+
			"capacity %d", settledBalance, channel.Capacity)
	}

	r.RemoveLink(channelPoint)
	_, height, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return fmt.Errorf("read height for external close: %w", err)
	}
	if height < 0 {
		return fmt.Errorf("invalid external close height %d", height)
	}
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:               channel.FundingOutpoint,
		ShortChanID:             channel.ShortChannelID,
		ChainHash:               channel.ChainHash,
		ClosingTXID:             closingTxID,
		RemotePub:               channel.IdentityPub,
		Capacity:                channel.Capacity,
		CloseHeight:             uint32(height),
		SettledBalance:          settledBalance,
		CloseType:               channeldb.CooperativeClose,
		IsPending:               false,
		RemoteCurrentRevocation: channel.RemoteCurrentRevocation,
		RemoteNextRevocation:    channel.RemoteNextRevocation,
		LocalChanConfig:         channel.LocalChanCfg,
	}
	if chanSync, syncErr := channel.ChanSyncMsg(); syncErr == nil {
		closeSummary.LastChanSyncMsg = chanSync
	}
	status := channeldb.ChanStatusRemoteCloseInitiator
	if localInitiated {
		status = channeldb.ChanStatusLocalCloseInitiator
	}
	if err := channel.CloseChannel(closeSummary, status); err != nil {
		return fmt.Errorf("archive externally closed channel: %w", err)
	}

	return r.forgetExternallyClosedChannel(channelPoint)
}

// forgetExternallyClosedChannel retires the pre-registered on-chain watcher.
// The Ark cooperative-close FSM is the terminal durability barrier here, so
// this cleanup intentionally does not invoke the force-close resolution hook.
func (r *Runtime) forgetExternallyClosedChannel(
	channelPoint wire.OutPoint) error {

	if r.onchain == nil {
		return nil
	}
	if err := r.onchain.ForgetChannel(channelPoint); err != nil {
		return fmt.Errorf("retire externally closed channel: %w", err)
	}

	return nil
}
