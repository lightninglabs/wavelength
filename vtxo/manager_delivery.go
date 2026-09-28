package vtxo

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/timeout"
)

// deliverManagerNotification keeps a child's receive loop available while
// its manager is waiting for a child Ask. A full manager mailbox transfers
// the exact notification to the timeout actor, whose callbacks never block.
// One timer entry owns each notification across all retries, with no parked
// sender goroutine. The durable VTXO state continues to own the reservation;
// the existing startup reconciliation recovers it after process shutdown.
func (a *VTXOActor) deliverManagerNotification(ctx context.Context,
	msg ManagerMsg) error {

	// Notifications belong to the persisted transition, not its receive
	// turn. Each destination still enforces its own shutdown lifecycle.
	notifyCtx := context.WithoutCancel(ctx)
	if a.cfg.TimeoutActor == nil {
		return a.cfg.Manager.Tell(ctx, msg)
	}
	err := a.cfg.Manager.TryTell(notifyCtx, msg)
	if err == nil || errors.Is(err, actor.ErrActorTerminated) ||
		errors.Is(err, actor.ErrMailboxClosed) {
		return err
	}

	// A retained leader can outlive the reservation that emitted it. Carry
	// a child-owned validity token to the manager, which checks it when the
	// delayed envelope is processed. Release, signing, or exit invalidates
	// the token before a newer reservation can reuse this outpoint.
	relay, isRelay := msg.(*RelayToRoundMsg)
	if isRelay {
		req, refresh := relay.Payload.(*round.RefreshVTXORequest)
		if refresh && req.Automatic {
			active := &atomic.Bool{}
			active.Store(true)
			a.pendingRefreshDelivery = active
			msg = &deferredRefreshRelay{
				relay:  relay,
				active: active,
			}
		}
	}
	callback := timeout.MapTimeoutExpired(
		a.cfg.Manager,
		func(timeout.ExpiredMsg) ManagerMsg { return msg },
	)

	// Unique IDs keep later notifications from replacing undelivered work.
	// The scheduler retains failures until delivery or manager shutdown.
	return a.cfg.TimeoutActor.Tell(
		notifyCtx, &timeout.ScheduleTimeoutRequest{
			ID: timeout.ID(
				"vtxo-manager-notification:" +
					uuid.NewString(),
			),
			Duration: time.Millisecond,
			Callback: callback,
		},
	)
}

// deferredRefreshRelay retains a notification only for the child reservation
// that emitted it. The manager checks active before consulting cohort markers,
// since a later height may already have replaced the marker for this outpoint.
type deferredRefreshRelay struct {
	actor.BaseMessage
	relay  *RelayToRoundMsg
	active *atomic.Bool
}

// MessageType identifies a retained automatic refresh notification.
func (*deferredRefreshRelay) MessageType() string {
	return "DeferredRefreshRelay"
}

// VTXOManagerMsg admits the retained envelope to the manager mailbox.
func (*deferredRefreshRelay) VTXOManagerMsg() {}
