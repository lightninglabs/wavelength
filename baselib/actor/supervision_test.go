package actor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// newSupervisedCodec builds a codec carrying both the actor test message and
// the framework's RestartMessage. A supervised restart enqueues the latter, so
// an actor that can restart must be able to decode it.
func newSupervisedCodec() *MessageCodec {
	codec := newActorTestCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	return codec
}

// supervisedBehavior is a classic behavior typed over the generic TLVMessage
// so it receives both the test message and the RestartMessage a supervised
// restart prepends. It panics on the test message for as long as the injected
// predicate says to, and records every restart checkpoint it is handed.
type supervisedBehavior struct {
	mu sync.Mutex

	// restarts records the checkpoint carried by each RestartMessage the
	// behavior has seen, in delivery order.
	restarts []fn.Option[Checkpoint]

	// values records the payload of each non-restart message received.
	values []uint64

	// shouldPanic decides whether the given test message panics. A nil
	// predicate never panics.
	shouldPanic func(value uint64) bool

	// onReceive runs before the panic decision, for tests that need to
	// observe or block inside the turn.
	onReceive func(ctx context.Context, value uint64)

	// stopCalls counts OnStop invocations, which supervision runs once per
	// restart plus once at final teardown.
	stopCalls atomic.Int32
}

// Receive implements ActorBehavior over the generic TLVMessage type.
func (b *supervisedBehavior) Receive(ctx context.Context,
	msg TLVMessage) fn.Result[int] {

	if restart, ok := msg.(*RestartMessage); ok {
		b.mu.Lock()
		b.restarts = append(b.restarts, restart.Checkpoint)
		b.mu.Unlock()

		return fn.Ok(0)
	}

	test, ok := msg.(*actorTestMsg)
	if !ok {
		return fn.Err[int](errors.New("unexpected message type"))
	}

	value := test.Value.Val

	b.mu.Lock()
	b.values = append(b.values, value)
	onReceive := b.onReceive
	shouldPanic := b.shouldPanic
	b.mu.Unlock()

	if onReceive != nil {
		onReceive(ctx, value)
	}

	if shouldPanic != nil && shouldPanic(value) {
		panic("supervised behavior panic")
	}

	return fn.Ok(int(value))
}

// OnStop implements Stoppable so the tests can observe that supervision tears
// the behavior down before restarting it.
func (b *supervisedBehavior) OnStop(context.Context) error {
	b.stopCalls.Add(1)

	return nil
}

// restartCount returns how many RestartMessages the behavior has seen.
func (b *supervisedBehavior) restartCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.restarts)
}

// lastRestart returns the checkpoint carried by the most recent
// RestartMessage.
func (b *supervisedBehavior) lastRestart() fn.Option[Checkpoint] {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.restarts) == 0 {
		return fn.None[Checkpoint]()
	}

	return b.restarts[len(b.restarts)-1]
}

// valueCount returns how many non-restart messages the behavior has seen.
func (b *supervisedBehavior) valueCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.values)
}

// newSupervisedActor builds a single-worker durable actor over a
// supervisedBehavior, with a fast poll so restarts are observable inside a
// test's patience.
func newSupervisedActor(t *testing.T, store DeliveryStore,
	behavior *supervisedBehavior,
	tweak func(*DurableActorConfig[TLVMessage, int]),
) *DurableActor[TLVMessage, int] {

	t.Helper()

	cfg := DefaultDurableActorConfig[TLVMessage, int](
		"supervised-actor", behavior, store, newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.CleanupTimeout = time.Second

	if tweak != nil {
		tweak(&cfg)
	}

	return NewDurableActor(cfg).UnwrapOrFail(t)
}

// tellSupervised enqueues a value-carrying test message.
func tellSupervised(t *testing.T, a *DurableActor[TLVMessage, int],
	value uint64) {

	t.Helper()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
	}

	require.NoError(t, a.Ref().Tell(context.Background(), msg))
}

// TestDurableActorPanicRestartsFromCheckpoint verifies that a panicking
// behavior is not merely nacked and re-fed: the actor tears the behavior down,
// reloads its persisted FSM checkpoint, and hands it back through a
// RestartMessage exactly as a process restart would.
func TestDurableActorPanicRestartsFromCheckpoint(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(value uint64) bool {
			return value == 1
		},
	}

	// Persist a checkpoint so the restart has real state to restore, which
	// is what a supervised restart must feed back to the behavior.
	require.NoError(
		t,
		store.SaveCheckpoint(
			context.Background(), CheckpointParams{
				ActorID:   "supervised-actor",
				StateType: "SupervisedState",
				StateData: []byte{0xDE, 0xAD},
				Version:   7,
			},
		),
	)

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			// Give up on the poison message immediately so the
			// restart is the only thing left to observe.
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return false, 0
			}
		},
	)

	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// The behavior was handed the persisted checkpoint, not a blank slate.
	checkpoint := behavior.lastRestart().UnwrapOrFail(t)
	require.Equal(t, "supervised-actor", checkpoint.ActorID)
	require.Equal(t, "SupervisedState", checkpoint.StateType)
	require.Equal(t, []byte{0xDE, 0xAD}, checkpoint.StateData)
	require.EqualValues(t, 7, checkpoint.Version)

	// The behavior was torn down before the rebuild.
	require.GreaterOrEqual(t, int(behavior.stopCalls.Load()), 1)

	// The actor is alive on the far side of the restart and still serving
	// its original identity: the same Ref reaches it, and a new message is
	// processed.
	tellSupervised(t, a, 2)

	require.Eventually(t, func() bool {
		return behavior.valueCount() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, a.ctx.Err())
}

// TestDurableActorPanicKeepsIdentityStable verifies the actor's public
// identity survives a restart: the Ref handed out before the panic is the same
// object afterwards, and it still reaches the actor.
func TestDurableActorPanicKeepsIdentityStable(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(value uint64) bool {
			return value == 1
		},
	}

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return false, 0
			}
		},
	)

	ref := a.Ref()
	mailbox := a.mailbox

	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	require.Same(t, mailbox, a.mailbox)
	require.Equal(t, ref, a.Ref())
	require.Equal(t, "supervised-actor", ref.ID())

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(9)),
	}
	require.NoError(t, ref.Tell(context.Background(), msg))

	require.Eventually(t, func() bool {
		return behavior.valueCount() >= 2
	}, 5*time.Second, 10*time.Millisecond)
}

// TestDurableActorBehaviorErrorDoesNotRestart verifies supervision only fires
// on a panic. A behavior that returns a failed result is an ordinary message
// failure, so the message retries and the actor is left alone.
func TestDurableActorBehaviorErrorDoesNotRestart(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{}
	behavior.onReceive = func(context.Context, uint64) {}

	failing := &failingSupervisedBehavior{supervisedBehavior: behavior}

	cfg := DefaultDurableActorConfig[TLVMessage, int](
		"supervised-actor", failing, store, newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.TellRetryPolicy = func(_ error, attempts int) (bool,
		time.Duration) {

		return attempts < 3, time.Millisecond
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	require.Eventually(t, func() bool {
		return behavior.valueCount() >= 3
	}, 5*time.Second, 10*time.Millisecond)

	// Retries happened, but no restart: the behavior never saw a
	// RestartMessage and the tracker stayed at zero.
	require.Zero(t, behavior.restartCount())
	require.Zero(t, a.restarts.count())
}

// failingSupervisedBehavior wraps supervisedBehavior and turns every test
// message into a failed result instead of a panic.
type failingSupervisedBehavior struct {
	*supervisedBehavior
}

// Receive records the message through the embedded behavior and then fails.
func (b *failingSupervisedBehavior) Receive(ctx context.Context,
	msg TLVMessage) fn.Result[int] {

	if _, ok := msg.(*RestartMessage); ok {
		return b.supervisedBehavior.Receive(ctx, msg)
	}

	b.supervisedBehavior.Receive(ctx, msg)

	return fn.Err[int](errors.New("behavior failed"))
}

// TestDurableActorPoisonMessageDeadLettersAcrossRestarts verifies the
// nack-before-restart ordering. A deterministically panicking message burns an
// attempt on every pass, so it climbs to max_attempts and dead-letters instead
// of restarting the actor forever.
func TestDurableActorPoisonMessageDeadLettersAcrossRestarts(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.MaxAttempts = 3
			cfg.MaxRestarts = 10
			cfg.TellRetryPolicy = func(_ error, attempts int) (bool,
				time.Duration) {

				return attempts < 3, time.Millisecond
			}
		},
	)

	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	// The poison message ends up in the dead letter queue rather than
	// crash-looping the actor.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		return len(store.deadLetters) >= 1
	}, 10*time.Second, 10*time.Millisecond)

	// Each pass panicked, so each pass restarted the actor: the attempts
	// budget, not the restart budget, is what stopped the loop.
	require.Equal(t, 3, behavior.valueCount())
	require.Equal(t, 3, a.restarts.count())

	// The actor survived, is still inside its restart budget, and keeps
	// serving traffic.
	require.NoError(t, a.ctx.Err())

	behavior.mu.Lock()
	behavior.shouldPanic = nil
	behavior.mu.Unlock()

	tellSupervised(t, a, 2)

	require.Eventually(t, func() bool {
		return behavior.valueCount() >= 4
	}, 5*time.Second, 10*time.Millisecond)
}

// TestDurableActorRestartIntensityTerminates verifies that a behavior which
// keeps panicking eventually exhausts its restart budget, at which point the
// actor is stopped permanently and its watchers are told why.
func TestDurableActorRestartIntensityTerminates(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.MaxRestarts = 2
			cfg.RestartWindow = time.Hour
			cfg.MaxAttempts = 100
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return true, time.Millisecond
			}
		},
	)

	watch := a.Watch(context.Background())

	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	var info TerminationInfo
	select {
	case info = <-watch:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for termination")
	}

	require.Equal(t, "supervised-actor", info.ActorID)
	require.Equal(
		t, TerminationRestartIntensityExceeded, info.Reason,
	)
	require.True(t, info.RestartsExhausted)
	require.Equal(t, 3, info.Restarts)
	require.Error(t, info.Err)
	require.True(t, isBehaviorPanic(info.Err))

	// The actor is terminal: it has finished shutting down and refuses
	// further work rather than accumulating a backlog nothing will drain.
	require.NoError(t, a.Wait(context.Background()))
	require.Error(t, a.ctx.Err())

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(2)),
	}
	require.Error(t, a.Ref().Tell(context.Background(), msg))

	// The watch channel carries exactly one notification and is then
	// closed.
	_, ok := <-watch
	require.False(t, ok)
}

// TestDurableActorWatchReportsGracefulStop verifies that an ordinary Stop is
// reported as such, with no restarts and no exhausted budget.
func TestDurableActorWatchReportsGracefulStop(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{}

	a := newSupervisedActor(t, store, behavior, nil)

	watch := a.Watch(context.Background())

	a.Start()
	tellSupervised(t, a, 1)

	require.Eventually(t, func() bool {
		return behavior.valueCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, a.StopAndWait(context.Background()))

	info := <-watch
	require.Equal(t, TerminationStopped, info.Reason)
	require.Equal(t, "stopped", info.Reason.String())
	require.NoError(t, info.Err)
	require.Zero(t, info.Restarts)
	require.False(t, info.RestartsExhausted)

	// Registering after the fact still yields the notification, so a
	// watcher cannot lose the race against a stopping actor.
	late := a.Watch(context.Background())
	require.Equal(t, info, <-late)

	_, ok := <-late
	require.False(t, ok)
}

// TestDurableActorWatchDoesNotBlockShutdown verifies the watch contract's
// never-park half: several watchers that never read must not hold up the
// actor's shutdown, and each still receives exactly one notification.
func TestDurableActorWatchDoesNotBlockShutdown(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{}

	a := newSupervisedActor(t, store, behavior, nil)

	// Deliberately never read from these before the shutdown.
	watches := make([]<-chan TerminationInfo, 0, 8)
	for i := 0; i < 8; i++ {
		watches = append(watches, a.Watch(context.Background()))
	}

	a.Start()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)

		a.Stop()
		_ = a.Wait(context.Background())
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown parked on an unread watcher")
	}

	// Every watcher gets exactly one notification, then a closed channel.
	for _, watch := range watches {
		info, ok := <-watch
		require.True(t, ok)
		require.Equal(t, TerminationStopped, info.Reason)

		_, ok = <-watch
		require.False(t, ok)
	}
}

// TestDurableActorWatchContextCancelDeregisters verifies a watcher that loses
// interest releases its registration: the channel closes with no notification.
func TestDurableActorWatchContextCancelDeregisters(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{}

	a := newSupervisedActor(t, store, behavior, nil)
	a.Start()
	defer a.Stop()

	watchCtx, cancel := context.WithCancel(context.Background())
	watch := a.Watch(watchCtx)
	cancel()

	select {
	case info, ok := <-watch:
		require.False(t, ok, "expected close, got %v", info)

	case <-time.After(5 * time.Second):
		t.Fatal("cancelled watcher was never released")
	}
}

// supervisedExecBehavior is a Read/Commit behavior typed over the generic
// TLVMessage, so a multi-worker pool (which is only valid on that path) can
// still receive the RestartMessage a supervised restart prepends.
type supervisedExecBehavior struct {
	mu sync.Mutex

	// restarts counts the RestartMessages the behavior has seen.
	restarts int

	// parked counts the turns that are currently blocked waiting for their
	// context to be cancelled.
	parked atomic.Int32

	// cancelled counts the parked turns that observed cancellation, which
	// is the evidence that a restart drained every worker.
	cancelled atomic.Int32

	// parking gates the parking behavior. It is cleared by the panicking
	// turn so the parked messages simply commit when they are redelivered.
	parking atomic.Bool

	// panics counts how many times the panic message has been delivered.
	// Only the first delivery panics, so the redelivered message does not
	// crash-loop the actor out of its restart budget.
	panics atomic.Int32

	// values counts the committed non-restart turns.
	values int
}

// Receive implements TxBehavior over the generic TLVMessage type. A message
// with value 0 parks until its context is cancelled, a message with value 1
// panics on its first delivery, and anything else commits straight away.
func (b *supervisedExecBehavior) Receive(ctx context.Context, msg TLVMessage,
	ax Exec[DeliveryStore]) fn.Result[int] {

	if _, ok := msg.(*RestartMessage); ok {
		b.mu.Lock()
		b.restarts++
		b.mu.Unlock()

		if err := ax.Commit(ctx, noOpCommit); err != nil {
			return fn.Err[int](err)
		}

		return fn.Ok(0)
	}

	test, ok := msg.(*actorTestMsg)
	if !ok {
		return fn.Err[int](errors.New("unexpected message type"))
	}

	switch {
	case test.Value.Val == 0 && b.parking.Load():
		b.parked.Add(1)
		<-ctx.Done()
		b.cancelled.Add(1)

		return fn.Err[int](ctx.Err())

	case test.Value.Val == 1 && b.panics.Add(1) == 1:
		// Release the parked turns from the panic itself, so the
		// restart is what cancels them rather than a test-side race.
		b.parking.Store(false)

		panic("supervised exec behavior panic")
	}

	if err := ax.Commit(ctx, noOpCommit); err != nil {
		return fn.Err[int](err)
	}

	b.mu.Lock()
	b.values++
	b.mu.Unlock()

	return fn.Ok(0)
}

// restartCount returns how many RestartMessages the behavior has seen.
func (b *supervisedExecBehavior) restartCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.restarts
}

// TestDurableActorMultiWorkerRestartDrainsPool verifies a restart stops every
// worker of a competing-consumer pool, not just the one that panicked, and
// brings the configured worker count back afterwards.
func TestDurableActorMultiWorkerRestartDrainsPool(t *testing.T) {
	t.Parallel()

	const numWorkers = 4

	store := newMockTxAwareStore()
	behavior := &supervisedExecBehavior{}
	behavior.parking.Store(true)

	cfg := DefaultDurableTxActorConfig[TLVMessage, int, DeliveryStore](
		"supervised-actor", behavior, identityStoreFactory, store,
		newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.NumWorkers = numWorkers
	cfg.MaxRestarts = 5
	cfg.MaxAttempts = 100
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return true, time.Millisecond
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	// Park three of the four workers.
	for i := 0; i < numWorkers-1; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(0)),
		}
		require.NoError(t, a.Ref().Tell(context.Background(), msg))
	}

	require.Eventually(t, func() bool {
		return behavior.parked.Load() == numWorkers-1
	}, 5*time.Second, 10*time.Millisecond)

	// Panic on the fourth.
	panicMsg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), panicMsg))

	// Every parked worker observed cancellation, so the restart drained
	// the whole pool rather than only the panicking worker.
	require.Eventually(t, func() bool {
		return behavior.cancelled.Load() == numWorkers-1
	}, 10*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 10*time.Second, 10*time.Millisecond)

	require.Equal(t, numWorkers, a.numWorkers)
	require.NoError(t, a.ctx.Err())
}

// TestDurableActorRestartWithoutRestartCodec verifies that an actor whose
// codec never registered the RestartMessage still restarts, but skips the
// checkpoint hand-off instead of enqueueing a message its own consumer cannot
// decode (which would dead-letter on every restart).
func TestDurableActorRestartWithoutRestartCodec(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := newMockBehavior(fn.Ok(42))
	behavior.panicOnReceive = true

	// newActorTestCodec deliberately carries no RestartMessage.
	cfg := DefaultDurableActorConfig(
		"test-actor", behavior, store, newActorTestCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return false, 0
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), msg))

	require.Eventually(t, func() bool {
		return a.restarts.count() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// The only dead letter is the poison message itself: no undecodable
	// restart message was enqueued behind it.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		return len(store.deadLetters) == 1
	}, 5*time.Second, 10*time.Millisecond)

	store.mu.Lock()
	for _, m := range store.messages {
		require.NotEqual(t, "actor.Restart", m.MessageType)
	}
	store.mu.Unlock()

	require.NoError(t, a.ctx.Err())
}

// TestRestartTrackerSlidingWindow verifies the intensity budget is a sliding
// window: restarts inside the window count against the budget, and restarts
// that have aged out do not.
func TestRestartTrackerSlidingWindow(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1000, 0))
	tracker := newRestartTracker(2, time.Minute, clk)

	require.True(t, tracker.record())
	require.True(t, tracker.record())

	// The third restart inside the window breaks the budget.
	require.False(t, tracker.record())
	require.Equal(t, 3, tracker.count())

	// Once the window has slid past the earlier restarts, the budget is
	// available again.
	clk.SetTime(clk.Now().Add(2 * time.Minute))
	require.True(t, tracker.record())
	require.Equal(t, 4, tracker.count())
}

// TestRestartTrackerUnlimited verifies the explicit opt-out never runs out of
// budget.
func TestRestartTrackerUnlimited(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1000, 0))
	tracker := newRestartTracker(UnlimitedRestarts, time.Minute, clk)

	for i := 0; i < 100; i++ {
		require.True(t, tracker.record())
	}

	require.Equal(t, 100, tracker.count())
}

// TestDurableActorRestartBudgetDefaults verifies the intensity budget defaults
// on: both the default config and a hand-built config that never mentions
// MaxRestarts land on the bounded default rather than an unlimited budget.
func TestDurableActorRestartBudgetDefaults(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newSupervisedCodec()
	behavior := &supervisedBehavior{}

	cfg := DefaultDurableActorConfig[TLVMessage, int](
		"a", behavior, store, codec,
	)
	require.Equal(t, DefaultMaxRestarts, cfg.MaxRestarts)
	require.Equal(t, DefaultRestartWindow, cfg.RestartWindow)

	// A hand-built config with a zero MaxRestarts is normalized to the
	// default, not to an unbounded crash loop.
	bare := DurableActorConfig[TLVMessage, int]{
		ID:       "a",
		Behavior: NewClassicBehavior[TLVMessage, int](behavior),
		Store:    store,
		Codec:    codec,
	}
	bareActor := NewDurableActor(bare).UnwrapOrFail(t)
	require.Equal(t, DefaultMaxRestarts, bareActor.restarts.max)
	require.Equal(t, DefaultRestartWindow, bareActor.restarts.window)

	// The opt-out is honored verbatim.
	cfg.MaxRestarts = UnlimitedRestarts
	unlimited := NewDurableActor(cfg).UnwrapOrFail(t)
	require.Equal(t, UnlimitedRestarts, unlimited.restarts.max)
}

// TestTerminationReasonString verifies every reason renders a stable name.
func TestTerminationReasonString(t *testing.T) {
	t.Parallel()

	require.Equal(t, "stopped", TerminationStopped.String())
	require.Equal(
		t, "context_cancelled", TerminationContextCancelled.String(),
	)
	require.Equal(
		t, "restart_intensity_exceeded",
		TerminationRestartIntensityExceeded.String(),
	)
	require.Equal(
		t, "restart_failed", TerminationRestartFailed.String(),
	)
	require.Equal(t, "unknown(9)", TerminationReason(9).String())
}
