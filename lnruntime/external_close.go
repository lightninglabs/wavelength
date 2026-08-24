package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const cleanChannelPollInterval = 25 * time.Millisecond

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

	channelID := lnwire.NewChanIDFromOutPoint(channelPoint)
	link, err := r.switcher.GetLink(channelID)
	switch {
	case err == nil:
		link.DisableAdds(htlcswitch.Incoming)
		link.DisableAdds(htlcswitch.Outgoing)

		flushed := make(chan struct{}, 1)
		link.OnFlushedOnce(func() {
			flushed <- struct{}{}
		})
		select {
		case <-ctx.Done():
			r.resumeChannelLink(channelPoint)

			return CleanChannelState{}, ctx.Err()

		case <-flushed:
		}

	case errors.Is(err, htlcswitch.ErrChannelLinkNotFound):
		// A restart may replay the close before links are restored.
		// With no live link, the persisted channel cannot accept new
		// updates and is safe to inspect directly.

	default:
		return CleanChannelState{}, fmt.Errorf("find channel link: %w",
			err)
	}

	ticker := time.NewTicker(cleanChannelPollInterval)
	defer ticker.Stop()
	for {
		state, clean, err := r.cleanChannelState(channelPoint)
		if err != nil {
			r.resumeChannelLink(channelPoint)

			return CleanChannelState{}, err
		}
		if clean {
			return state, nil
		}

		select {
		case <-ctx.Done():
			r.resumeChannelLink(channelPoint)

			return CleanChannelState{}, ctx.Err()

		case <-ticker.C:
		}
	}
}

// ResumeChannel re-enables both directions after a cooperative close attempt
// fails before the fully signed VTXO spend becomes durable.
func (r *Runtime) ResumeChannel(channelPoint wire.OutPoint) {
	r.resumeChannelLink(channelPoint)
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

// cleanChannelState reconstructs lnd's channel state and returns the exact
// zero-fee cooperative balances once both commitment chains are synchronized.
func (r *Runtime) cleanChannelState(channelPoint wire.OutPoint) (
	CleanChannelState, bool, error) {

	state, err := r.cfg.DB.ChannelStateDB().FetchChannel(channelPoint)
	if err != nil {
		return CleanChannelState{}, false, fmt.Errorf("find channel "+
			"for cooperative close: %w", err)
	}
	if state.IsPending {
		return CleanChannelState{}, false, fmt.Errorf("cannot " +
			"cooperatively close a pending channel")
	}
	channel, err := lnwallet.NewLightningChannel(
		r.cfg.Signer, state, r.sigPool,
	)
	if err != nil {
		return CleanChannelState{}, false, fmt.Errorf("restore "+
			"channel for cooperative close: %w", err)
	}
	if !channel.IsChannelClean() {
		return CleanChannelState{}, false, nil
	}
	if state.LocalCommitment.CommitHeight !=
		state.RemoteCommitment.CommitHeight {
		return CleanChannelState{}, false, nil
	}
	localBalance, remoteBalance, err := lnwallet.CoopCloseBalance(
		state.ChanType, state.IsInitiator, 0,
		state.LocalCommitment.LocalBalance.ToSatoshis(),
		state.LocalCommitment.RemoteBalance.ToSatoshis(),
		state.LocalCommitment.CommitFee,
		fn.None[lntypes.ChannelParty](),
	)
	if err != nil {
		return CleanChannelState{}, false, fmt.Errorf("derive "+
			"cooperative close balances: %w", err)
	}
	if localBalance+remoteBalance != state.Capacity {
		return CleanChannelState{}, false, fmt.Errorf("cooperative "+
			"balances %d + %d do not match capacity %d",
			localBalance, remoteBalance, state.Capacity)
	}

	return CleanChannelState{
		ChannelPoint:     channelPoint,
		LocalBalance:     localBalance,
		RemoteBalance:    remoteBalance,
		Capacity:         state.Capacity,
		CommitmentHeight: state.LocalCommitment.CommitHeight,
		LocalInitiator:   state.IsInitiator,
	}, true, nil
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
