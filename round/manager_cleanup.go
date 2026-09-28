package round

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/timeout"
)

// deliverForfeitRelease hands a failed round's reservation release to the
// manager without waiting for its mailbox to drain. The manager may be waiting
// to relay into this actor's full mailbox. On backpressure the timeout actor
// owns the exact request, retrying until delivery or manager shutdown. It never
// waits on a callback, so scheduling through it cannot close that circular
// wait.
//
// Each cleanup owns one timer entry, regardless of retry count. Reservations
// remain held until delivery; restart uses the existing orphan-forfeit sweep.
func (a *RoundClientActor) deliverForfeitRelease(ctx context.Context,
	msg *actormsg.ReleaseForfeitRequest) error {

	// Cleanup belongs to the failed round, not the request that observed
	// failure. Both destinations enforce their own shutdown lifecycle.
	cleanupCtx := context.WithoutCancel(ctx)
	err := a.cfg.VTXOManager.TryTell(cleanupCtx, msg)
	if err == nil || errors.Is(err, actor.ErrActorTerminated) ||
		errors.Is(err, actor.ErrMailboxClosed) {
		return err
	}
	if a.cfg.TimeoutActor == nil {
		return fmt.Errorf("retain manager cleanup: no timeout "+
			"actor: %w", err)
	}

	callback := timeout.MapTimeoutExpired(
		a.cfg.VTXOManager,
		func(timeout.ExpiredMsg) VTXOManagerMsg {
			return msg
		},
	)

	// A unique ID prevents one failed round release from replacing
	// another pending callback. Ordinary round timeout cancellation cannot
	// cancel this obligation.
	return a.cfg.TimeoutActor.Tell(
		cleanupCtx, &timeout.ScheduleTimeoutRequest{
			ID: timeout.ID(
				"round-forfeit-release:" + uuid.NewString(),
			),
			Duration: time.Millisecond,
			Callback: callback,
		},
	)
}
