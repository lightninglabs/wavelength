package vtxo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// deliveryClock exposes scheduled callbacks so tests can hold mailbox pressure
// across repeated delivery attempts without depending on wall-clock sleeps.
type deliveryClock struct {
	timers chan *deliveryTimer
}

// Now returns a fixed instant; these tests use relative timeout durations.
func (c *deliveryClock) Now() time.Time { return time.Unix(0, 0) }

// AfterFunc retains one callback until the test fires it or shutdown stops it.
func (c *deliveryClock) AfterFunc(_ time.Duration, f func()) timeout.Stoppable {
	timer := &deliveryTimer{callback: f}
	c.timers <- timer

	return timer
}

// deliveryTimer tracks cancellation of one manually driven timeout.
type deliveryTimer struct {
	mu       sync.Mutex
	stopped  bool
	callback func()
}

// Stop prevents a pending manual fire and reports whether it was still live.
func (c *deliveryTimer) Stop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasLive := !c.stopped
	c.stopped = true

	return wasLive
}

// fire executes a live callback exactly once, like time.AfterFunc.
func (c *deliveryTimer) fire() {
	c.mu.Lock()
	live := !c.stopped
	c.stopped = true
	c.mu.Unlock()
	if live {
		c.callback()
	}
}

// nextDeliveryTimer waits for the timeout actor to retain a retry callback.
func nextDeliveryTimer(t *testing.T, clock *deliveryClock) *deliveryTimer {
	t.Helper()
	select {
	case timer := <-clock.timers:
		return timer

	case <-time.After(2 * time.Second):
		t.Fatal("manager notification was not retained")

		return nil
	}
}

// TestManagerNotificationRetryLifecycle holds real mailbox pressure over
// repeated retries, cancels the producing turn, and proves exact-message
// delivery after pressure clears. Manager or scheduler shutdown stops retry.
func TestManagerNotificationRetryLifecycle(t *testing.T) {
	for _, mode := range []string{
		"drain",
		"manager shutdown",
		"scheduler shutdown",
	} {
		t.Run(mode, func(t *testing.T) {
			testManagerNotificationRetryLifecycle(t, mode)
		})
	}
}

// testManagerNotificationRetryLifecycle exercises one scheduler lifecycle
// with manually fired retries and a real one-slot target mailbox.
func testManagerNotificationRetryLifecycle(t *testing.T, mode string) {
	t.Helper()
	t.Parallel()
	clock := &deliveryClock{
		timers: make(chan *deliveryTimer, 4),
	}
	timer := timeout.NewActorWithClock(clock)
	var wg sync.WaitGroup
	timerActor := actor.NewActor(actor.ActorConfig[
		timeout.Msg,
		timeout.Resp,
	]{
		ID:          "notification-timer",
		Behavior:    timer,
		MailboxSize: 1,
		Wg:          &wg,
	})
	timer.Start(timerActor.TellRef())
	timerActor.Start()
	received := make(chan ManagerMsg, 4)
	manager := actor.NewActor(actor.ActorConfig[
		ManagerMsg,
		ManagerResp,
	]{
		ID: "notification-manager", MailboxSize: 1, Wg: &wg,
		Behavior: actor.NewFunctionBehavior(
			func(_ context.Context,
				msg ManagerMsg) fn.Result[ManagerResp] {

				received <- msg

				return fn.Ok[ManagerResp](nil)
			},
		),
	})
	t.Cleanup(
		func() {
			manager.Stop()
			timerActor.Stop()
			wg.Wait()
		},
	)
	child := &VTXOActor{cfg: &VTXOActorConfig{
		Manager: manager.TellRef(), TimeoutActor: timerActor.TellRef(),
	}}
	require.NoError(
		t,
		manager.Ref().TryTell(
			t.Context(),
			&GetActiveVTXOCountRequest{},
		),
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := &RelayToRoundMsg{Payload: &round.RefreshVTXORequest{
		TriggerHeight: 800, Amount: 10000,
	}}
	require.NoError(
		t, child.deliverManagerNotification(ctx, req),
	)
	for range 3 {
		nextDeliveryTimer(t, clock).fire()
	}
	pending := nextDeliveryTimer(t, clock)
	switch mode {
	case "manager shutdown":
		manager.Stop()
		pending.fire()
		probeCtx, cancel := context.WithTimeout(
			t.Context(), time.Second,
		)
		defer cancel()
		_, err := timerActor.Ref().Ask(probeCtx,
			&timeout.CancelTimeoutRequest{ID: "barrier"},
		).Await(probeCtx).Unpack()
		require.NoError(t, err)
		require.Empty(t, clock.timers)
		require.Empty(t, received)

	case "scheduler shutdown":
		timerActor.Stop()
		wg.Wait()
		pending.fire()
		require.Empty(t, clock.timers)
		require.Empty(t, received)

	default:
		manager.Start()
		probeCtx, cancel := context.WithTimeout(
			t.Context(), time.Second,
		)
		defer cancel()
		select {
		case <-received:
		case <-probeCtx.Done():
			t.Fatal("manager filler did not drain")
		}
		pending.fire()
		select {
		case got := <-received:
			require.Same(t, req, got)

		case <-time.After(time.Second):
			t.Fatal("retained notification lost")
		}
		// Normal flow uses the immediate handoff and
		// creates no timer.
		signature := &RelayToRoundMsg{
			Payload: &round.ForfeitSignatureResponse{},
		}
		require.NoError(
			t, child.deliverManagerNotification(
				ctx, signature,
			),
		)
		select {
		case got := <-received:
			require.Same(t, signature, got)

		case <-time.After(time.Second):
			t.Fatal("normal notification lost")
		}
		require.Empty(t, clock.timers)
	}
}

// TestDeferredRefreshRelayCannotOutliveReservation releases a reservation
// while its original relay is retained, then starts a later-height retry.
// Delivering the old timer last must neither submit the old request nor clear
// ownership of the replacement reservation.
func TestDeferredRefreshRelayCannotOutliveReservation(t *testing.T) {
	t.Parallel()
	desc := deterministicCohortDescriptor(t, 1, 1000)
	store := &MockVTXOStore{}
	store.On("UpdateVTXOStatus", mock.Anything, desc.Outpoint,
		VTXOStatusPendingForfeit).Return(nil).Twice()
	store.On("UpdateVTXOStatus", mock.Anything, desc.Outpoint,
		VTXOStatusLive).Return(nil).Once()
	clock := &deliveryClock{timers: make(chan *deliveryTimer, 4)}
	timer := timeout.NewActorWithClock(clock)
	var wg sync.WaitGroup
	timerActor := actor.NewActor(actor.ActorConfig[
		timeout.Msg,
		timeout.Resp,
	]{
		ID: "fence-timer", Behavior: timer, MailboxSize: 1, Wg: &wg,
	})
	timer.Start(timerActor.TellRef())
	timerActor.Start()
	received := make(chan ManagerMsg, 4)
	target := actor.NewActor(actor.ActorConfig[ManagerMsg, ManagerResp]{
		ID: "fence-manager", MailboxSize: 1, Wg: &wg,
		Behavior: actor.NewFunctionBehavior(func(_ context.Context,
			msg ManagerMsg) fn.Result[ManagerResp] {

			received <- msg

			return fn.Ok[ManagerResp](nil)
		}),
	})
	t.Cleanup(func() { target.Stop(); timerActor.Stop(); wg.Wait() })
	require.NoError(
		t,
		target.Ref().TryTell(t.Context(), &GetActiveVTXOCountRequest{}),
	)
	child := NewVTXOActor(t.Context(), &VTXOActorConfig{
		VTXO: desc, Store: store, ExpiryConfig: DefaultExpiryConfig(),
		Manager: target.TellRef(), TimeoutActor: timerActor.TellRef(),
	})
	const firstHeight = int32(800)
	_, err := child.
		Receive(t.Context(), &BlockEpochEvent{Height: firstHeight}).
		Unpack()
	require.NoError(t, err)
	old := nextDeliveryTimer(t, clock)
	oldToken := child.pendingRefreshDelivery
	require.True(t, oldToken.Load())
	_, err = child.Receive(t.Context(), &ForfeitReleasedEvent{}).Unpack()
	require.NoError(t, err)
	require.False(t, oldToken.Load())
	const retryHeight = firstHeight + autoRefreshRetryDelayBlocks
	_, err = child.
		Receive(t.Context(), &BlockEpochEvent{Height: retryHeight}).
		Unpack()
	require.NoError(t, err)
	latest := nextDeliveryTimer(t, clock)
	require.True(t, child.pendingRefreshDelivery.Load())
	target.Start()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("filler did not drain")
	}
	rounds := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{RoundActor: rounds})
	for _, pending := range []*deliveryTimer{latest, old} {
		pending.fire()
		select {
		case envelope := <-received:
			_, err := mgr.Receive(t.Context(), envelope).Unpack()
			require.NoError(t, err)
			// Model the later marker retained by cohort ownership.
			// An older retained relay must be rejected before
			// consulting this map.
			mgr.rememberAdoptedAutoRefreshRelay(
				autoRefreshLeaderRequest(desc, retryHeight),
			)

		case <-time.After(time.Second):
			t.Fatal("deferred relay was not delivered")
		}
	}
	require.Len(t, rounds.getMessages(), 1)
	cohort, ok := rounds.getMessages()[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 1)
	require.Equal(t, retryHeight, cohort.Requests[0].TriggerHeight)
	require.Equal(t, int64(desc.Amount), cohort.Requests[0].Amount)
	require.True(t, child.pendingRefreshDelivery.Load())
	require.Equal(
		t, retryHeight, mgr.adoptedAutoRefreshRelays[desc.Outpoint],
	)
	store.AssertExpectations(t)
}
