package round

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestFailedRoundCleanupBreaksMailboxCycle fills both peer mailboxes before
// failed-round cleanup crosses a manager-to-round refresh relay. The old
// blocking cleanup parks both receive loops permanently. Cleanup must instead
// retain the exact reservation owner and let both queued work and refresh
// drain.
func TestFailedRoundCleanupBreaksMailboxCycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	gate := make(chan struct{})
	roundEntered := make(chan struct{})
	managerEntered := make(chan struct{})
	cleanupDone := make(chan error, 1)
	refreshDone := make(chan struct{}, 1)
	releases := make(chan *actormsg.ReleaseForfeitRequest, 2)
	var wg sync.WaitGroup

	timer := timeout.NewActor()
	timerActor := actor.NewActor(actor.ActorConfig[
		timeout.Msg,
		timeout.Resp,
	]{
		ID: "cleanup-timer", Behavior: timer, MailboxSize: 1, Wg: &wg,
	})
	timer.Start(timerActor.TellRef())
	timerActor.Start()

	// Produce cleanup through the real rejected-round transition.
	forfeits, outpoints := frcForfeits()
	roundID := testRoundID("cleanup-deadlock")
	state := &RoundJoinedState{
		RoundID: roundID,
		Intents: Intents{
			Forfeits: forfeits,
		},
	}
	transition, err := state.ProcessEvent(ctx, &BoardingFailed{
		Reason: "output below minimum", Recoverable: true,
		Error: errors.New("output below minimum"),
	}, &ClientEnvironment{Log: btclog.Disabled})
	require.NoError(t, err)
	release, ok := findOutbox[*ReleaseForfeitReservation](
		transition.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
	)
	require.True(t, ok)

	client := &RoundClientActor{
		cfg: &RoundClientConfig{
			TimeoutActor: timerActor.TellRef(),
		},
		log: btclog.Disabled,
	}
	roundActor := actor.NewActor(actor.ActorConfig[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	]{
		ID: "cleanup-round", MailboxSize: 1, Wg: &wg,
		Behavior: actor.NewFunctionBehavior(func(ctx context.Context,
			msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

			switch msg.(type) {
			case *CancelRoundRequest:
				close(roundEntered)
				<-gate
				cleanupDone <- client.processOutbox(
					ctx, []ClientOutMsg{release},
				)

			case *RefreshVTXORequest:
				refreshDone <- struct{}{}
			}

			return fn.Ok[actormsg.RoundActorResp](nil)
		}),
	})
	managerActor := actor.NewActor(actor.ActorConfig[
		VTXOManagerMsg, actormsg.VTXOManagerResp,
	]{
		ID: "cleanup-manager", MailboxSize: 1, Wg: &wg,
		Behavior: actor.NewFunctionBehavior(func(
			ctx context.Context, msg VTXOManagerMsg,
		) fn.Result[actormsg.VTXOManagerResp] {

			req, ok := msg.(*actormsg.ReleaseForfeitRequest)
			if !ok {
				return fn.Errf[actormsg.VTXOManagerResp](
					"unexpected %T", msg,
				)
			}
			if req.RoundID == "relay" {
				close(managerEntered)
				<-gate
				// Match Manager.handleRelayToRound's blocking,
				// detached send. This edge remains unchanged.
				err := roundActor.Ref().Tell(
					context.WithoutCancel(ctx),
					&RefreshVTXORequest{},
				)

				return fn.NewResult[actormsg.VTXOManagerResp](
					nil, err,
				)
			}
			releases <- req

			return fn.Ok[actormsg.VTXOManagerResp](nil)
		}),
	})
	client.cfg.VTXOManager = managerActor.TellRef()
	t.Cleanup(func() {
		roundActor.Stop()
		managerActor.Stop()
		timerActor.Stop()
		wg.Wait()
	})
	roundActor.Start()
	managerActor.Start()
	require.NoError(t, roundActor.Ref().Tell(ctx, &CancelRoundRequest{}))
	require.NoError(
		t,
		managerActor.Ref().Tell(ctx, &actormsg.ReleaseForfeitRequest{
			RoundID: "relay",
		},
		),
	)
	<-roundEntered
	<-managerEntered
	require.NoError(
		t,
		roundActor.Ref().TryTell(ctx, &GetClientStateRequest{}),
	)
	require.NoError(
		t,
		managerActor.Ref().TryTell(ctx, &actormsg.ReleaseForfeitRequest{
			RoundID: "queued",
		},
		),
	)
	close(gate)

	select {
	case err := <-cleanupDone:
		require.NoError(t, err)

	case <-time.After(2 * time.Second):
		t.Fatal("failed-round cleanup is parked in the mailbox cycle")
	}
	select {
	case <-refreshDone:
	case <-time.After(2 * time.Second):
		t.Fatal("manager refresh relay did not drain")
	}
	for range 2 {
		select {
		case req := <-releases:
			if req.RoundID == "queued" {
				continue
			}
			require.Equal(t, roundID.String(), req.RoundID)
			require.Equal(t, outpoints, req.Outpoints)

		case <-time.After(3 * time.Second):
			t.Fatal("retained cleanup did not reach manager")
		}
	}
}

// cleanupClock exposes scheduled callbacks so tests can hold mailbox pressure
// across repeated delivery attempts without depending on wall-clock sleeps.
type cleanupClock struct {
	timers chan *cleanupTimer
}

// Now returns a fixed instant; these tests use relative timeout durations.
func (c *cleanupClock) Now() time.Time { return time.Unix(0, 0) }

// AfterFunc retains one callback until the test fires it or shutdown stops it.
func (c *cleanupClock) AfterFunc(_ time.Duration, f func()) timeout.Stoppable {
	timer := &cleanupTimer{callback: f}
	c.timers <- timer

	return timer
}

// cleanupTimer tracks cancellation of one manually driven timeout.
type cleanupTimer struct {
	mu       sync.Mutex
	stopped  bool
	callback func()
}

// Stop prevents a pending manual fire and reports whether it was still live.
func (c *cleanupTimer) Stop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasLive := !c.stopped
	c.stopped = true

	return wasLive
}

// fire executes a live callback exactly once, like time.AfterFunc.
func (c *cleanupTimer) fire() {
	c.mu.Lock()
	live := !c.stopped
	c.stopped = true
	c.mu.Unlock()
	if live {
		c.callback()
	}
}

// nextCleanupTimer waits for the timeout actor to retain a retry callback.
func nextCleanupTimer(t *testing.T, clock *cleanupClock) *cleanupTimer {
	t.Helper()
	select {
	case timer := <-clock.timers:
		return timer

	case <-time.After(2 * time.Second):
		t.Fatal("cleanup callback was not retained")

		return nil
	}
}

// TestManagerCleanupRetryLifecycle proves repeated saturation retains the same
// request despite caller cancellation. Recovery delivers it once; shutdown
// stops retry delivery instead of leaving a parked delivery goroutine.
func TestManagerCleanupRetryLifecycle(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "drain"
		if shutdown {
			name = "shutdown"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := &cleanupClock{
				timers: make(chan *cleanupTimer, 4),
			}
			timer := timeout.NewActorWithClock(clock)
			var wg sync.WaitGroup
			timerActor := actor.NewActor(actor.ActorConfig[
				timeout.Msg,
				timeout.Resp,
			]{
				ID:          "retry-timer",
				Behavior:    timer,
				MailboxSize: 1,
				Wg:          &wg,
			})
			timer.Start(timerActor.TellRef())
			timerActor.Start()
			manager := newMockVTXOManagerRef(t)
			behavior := actor.NewFunctionBehavior(
				func(ctx context.Context,
					msg VTXOManagerMsg) fn.Result[bool] {

					return fn.NewResult(
						true, manager.Tell(ctx, msg),
					)
				},
			)
			managerActor := actor.NewActor(actor.ActorConfig[
				VTXOManagerMsg, bool,
			]{
				ID: "retry-manager", MailboxSize: 1, Wg: &wg,
				Behavior: behavior,
			})
			t.Cleanup(func() {
				managerActor.Stop()
				timerActor.Stop()
				wg.Wait()
			})
			client := &RoundClientActor{
				cfg: &RoundClientConfig{
					VTXOManager:  managerActor.TellRef(),
					TimeoutActor: timerActor.TellRef(),
				},
			}
			require.NoError(
				t,
				managerActor.Ref().TryTell(t.Context(),
					&actormsg.ReleaseForfeitRequest{
						RoundID: "filler",
					},
				),
			)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, outpoints := frcForfeits()
			req := &actormsg.ReleaseForfeitRequest{
				RoundID: "failed-owner", Outpoints: outpoints,
			}
			require.NoError(
				t, client.deliverForfeitRelease(ctx, req),
			)
			for range 3 {
				nextCleanupTimer(t, clock).fire()
			}
			pending := nextCleanupTimer(t, clock)

			if shutdown {
				timerActor.Stop()
				wg.Wait()
				pending.fire()
				require.Empty(
					t, clock.timers,
					"shutdown must stop retries",
				)

				return
			}

			managerActor.Start()
			// A completed Ask proves the filler drained before
			// retry.
			_, err := managerActor.Ref().Ask(t.Context(),
				&actormsg.ReleaseForfeitRequest{
					RoundID: "barrier",
				},
			).Await(t.Context()).Unpack()
			require.NoError(t, err)
			pending.fire()
			require.Eventually(t, func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()

				return len(manager.messages) == 3
			}, 2*time.Second, time.Millisecond)
			manager.mu.Lock()
			require.Same(t, req, manager.messages[2])
			manager.mu.Unlock()
		})
	}
}
