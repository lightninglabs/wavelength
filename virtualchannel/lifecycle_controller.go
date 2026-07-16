package virtualchannel

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/funding"
)

const channelActivationPollInterval = 100 * time.Millisecond

// ChannelActivationLookup resolves both stages of a virtual channel's durable
// lifecycle. Pending IDs cover the interval before the channel point is
// registered; channel points cover restarts after lnd's funding flow completes.
type ChannelActivationLookup interface {
	ChannelPointLookup
	PendingChannelLookup

	// MarkVirtualChannelFundingVerified records that lnd reached its
	// durable pending-channel boundary for the exact funding outpoint
	// supplied to the activation gate.
	MarkVirtualChannelFundingVerified(context.Context, ID) (bool, error)
}

// ChannelStatusLookup loads one durable virtual channel by its stable ID.
type ChannelStatusLookup interface {
	GetVirtualChannel(context.Context, ID) (*Channel, error)
}

// LifecycleActivationGate keeps lnd's channel link behind the durable virtual
// channel FSM. Ordinary lnd channels pass through because they have no matching
// pending ID or channel point.
type LifecycleActivationGate struct {
	store ChannelActivationLookup
}

// NewLifecycleActivationGate creates the lnd channel activation adapter.
func NewLifecycleActivationGate(store ChannelActivationLookup) (
	*LifecycleActivationGate, error) {

	if store == nil {
		return nil, fmt.Errorf("virtual channel store is nil")
	}

	return &LifecycleActivationGate{store: store}, nil
}

// WaitForActivation blocks lnd before it marks the channel open or sends
// channel_ready. The wait survives RPC cancellation and is released by the
// durable FSM or lnd shutdown.
func (c *LifecycleActivationGate) WaitForActivation(ctx context.Context,
	req funding.ChannelActivationRequest) error {

	if ctx == nil {
		return fmt.Errorf("activation context is nil")
	}
	pendingID := PendingChannelID(req.PendingChanID)
	ticker := time.NewTicker(channelActivationPollInterval)
	defer ticker.Stop()

	for {
		status, virtual, err := c.activationStatus(
			ctx, pendingID, req.FundingOutpoint,
		)
		if err != nil {
			select {
			case <-ticker.C:
				continue

			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !virtual {
			return nil
		}
		if IsRoutableStatus(status) {
			return nil
		}
		switch status {
		case StatusFailed, StatusClosing, StatusMaterializing,
			StatusClosed:
			return fmt.Errorf("virtual channel cannot "+
				"activate from %s", status)

		default:
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *LifecycleActivationGate) activationStatus(ctx context.Context,
	pendingID PendingChannelID, fundingOutpoint wire.OutPoint) (Status,
	bool, error) {

	channel, ok, err := c.store.FindVirtualChannelByChannelPoint(
		ctx, fundingOutpoint,
	)
	if err != nil {
		return "", false, fmt.Errorf("lookup virtual channel %s: %w",
			fundingOutpoint, err)
	}
	if ok {
		if channel.Status == StatusLNDNegotiating {
			_, err := c.store.MarkVirtualChannelFundingVerified(
				ctx, channel.ID,
			)
			if err != nil {
				return "", true, fmt.Errorf("record lnd "+
					"funding for virtual channel %x: %w",
					channel.ID, err)
			}

			// Reload after the compare-and-swap. Another lifecycle
			// event may have advanced the row concurrently.
			channel, ok, err =
				c.store.FindVirtualChannelByChannelPoint(
					ctx, fundingOutpoint,
				)
			if err != nil {
				return "", true, fmt.Errorf("reload virtual "+
					"channel %s: %w", fundingOutpoint, err)
			}
			if !ok {
				return "", true, fmt.Errorf("virtual channel "+
					"%s disappeared", fundingOutpoint)
			}
		}

		return channel.Status, true, nil
	}

	pending, ok, err := c.store.FindVirtualChannelPendingOpen(
		ctx, pendingID,
	)
	if err != nil {
		return "", false, fmt.Errorf("lookup pending virtual channel "+
			"%x: %w", pendingID, err)
	}
	if !ok {
		return "", false, nil
	}

	return pending.Status, true, nil
}

// WaitForFundingVerified blocks channel negotiation until the LND activation
// hook has proven that the exact pending channel is durable. Later states are
// accepted so callers can safely replay after a crash.
func WaitForFundingVerified(ctx context.Context, store ChannelStatusLookup,
	id ID) (*Channel, error) {

	if store == nil {
		return nil, fmt.Errorf("virtual channel store is nil")
	}

	ticker := time.NewTicker(channelActivationPollInterval)
	defer ticker.Stop()

	for {
		channel, err := store.GetVirtualChannel(ctx, id)
		if err != nil {
			select {
			case <-ticker.C:
				continue

			case <-ctx.Done():
				return nil, fmt.Errorf("load virtual channel "+
					"%x: %w", id, err)
			}
		}

		switch channel.Status {
		case StatusFundingVerified, StatusBackingArmed,
			StatusRoundConfirmed, StatusActive,
			StatusFundingPublished:
			return channel, nil

		case StatusLNDNegotiating:
			// The activation hook owns the next edge.

		case StatusMaterializing, StatusClosing, StatusClosed,
			StatusFailed:
			return nil, fmt.Errorf("virtual channel %x cannot "+
				"continue funding from %s", id, channel.Status)

		default:
			return nil, fmt.Errorf("virtual channel %x cannot "+
				"verify lnd funding from %s", id,
				channel.Status)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

var _ funding.ChannelActivationGate = (*LifecycleActivationGate)(nil)
