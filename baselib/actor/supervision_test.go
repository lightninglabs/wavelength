package actor

import (
	"context"
	"encoding/binary"
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

	// failRestarts makes the RestartMessage handler fail, which is the
	// shape that would otherwise strand or pile up restart rows.
	failRestarts bool

	// stopCalls counts OnStop invocations, which supervision runs once per
	// restart plus once at final teardown.
	stopCalls atomic.Int32

	// stopPanics makes OnStop panic. Supervision must recover it rather
	// than let a cleanup that tripped over the same corrupt state take the
	// process down.
	stopPanics atomic.Bool
}

// Receive implements ActorBehavior over the generic TLVMessage type.
func (b *supervisedBehavior) Receive(ctx context.Context,
	msg TLVMessage) fn.Result[int] {

	if restart, ok := msg.(*RestartMessage); ok {
		b.mu.Lock()
		b.restarts = append(b.restarts, restart.Checkpoint)
		failRestarts := b.failRestarts
		b.mu.Unlock()

		if failRestarts {
			return fn.Err[int](errors.New("restore failed"))
		}

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

	if b.stopPanics.Load() {
		panic("supervised behavior cleanup panic")
	}

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

	// panicBarrier, when set, holds each panicking turn until every
	// participant has arrived, so several workers panic inside the SAME
	// generation rather than one per restart.
	panicBarrier *sync.WaitGroup

	// barrierArrivals counts turns that reached the barrier. Only the
	// first barrierSize of them panic; the redelivered messages commit so
	// the actor settles after exactly one restart.
	barrierArrivals atomic.Int32

	// barrierSize is how many turns the barrier waits for.
	barrierSize int32

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

	case test.Value.Val == 1 && b.panicBarrier != nil:
		// Wait for every participant to arrive so the panics land in
		// one generation rather than one per restart. Only the first
		// pass panics; the redelivered messages fall through to the
		// Commit below so the actor settles after one restart.
		if b.barrierArrivals.Add(1) <= b.barrierSize {
			b.panics.Add(1)
			b.panicBarrier.Done()
			b.panicBarrier.Wait()

			panic("supervised exec behavior concurrent panic")
		}

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

// TestDurableActorRestartWithoutRestartCodec verifies the degraded restart for
// an actor whose codec never registered the RestartMessage. Such an actor
// cannot be handed its checkpoint back, so the restart cycles the worker
// generation and otherwise leaves the behavior alone: no undecodable message
// is enqueued behind the poison one, and crucially no mid-life OnStop is run
// against a behavior instance that is reused anyway and gets no state rebuild
// out of the deal.
func TestDurableActorRestartWithoutRestartCodec(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := newStoppableMockBehavior(fn.Ok(42))
	behavior.panicOnReceive = true

	// newActorTestCodec deliberately carries no RestartMessage.
	cfg := DefaultDurableActorConfig[*actorTestMsg, int](
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

	// The behavior was never torn down mid-life, because a teardown it
	// cannot be rebuilt from is strictly worse than leaving it running.
	require.False(t, behavior.stopCalled.Load())

	require.NoError(t, a.ctx.Err())

	// The terminal stop still runs the hook exactly once.
	require.NoError(t, a.StopAndWait(context.Background()))
	require.True(t, behavior.stopCalled.Load())
	require.Equal(t, int32(1), behavior.stopCount.Load())
}

// TestDurableActorRestartRacingStop verifies that a Stop landing while a
// restart is in flight ends the actor gracefully rather than being reported as
// a supervision failure, and that StopAndWait still returns.
func TestDurableActorRestartRacingStop(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.MaxAttempts = 100
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return true, time.Millisecond
			}
		},
	)

	watch := a.Watch(context.Background())

	a.Start()
	tellSupervised(t, a, 1)

	// Let the restart loop get going, then stop into the middle of it.
	require.Eventually(t, func() bool {
		return a.restarts.count() >= 1
	}, 5*time.Second, time.Millisecond)

	require.NoError(t, a.StopAndWait(context.Background()))

	info := <-watch
	require.Equal(t, TerminationStopped, info.Reason)
	require.False(t, info.RestartsExhausted)
	require.NoError(t, info.Err)
}

// TestDurableActorRestartFailedTerminates verifies the TerminationRestartFailed
// path: a restart that is within budget but cannot be carried out (here the
// checkpoint load fails) terminates the actor and reports the failure rather
// than restarting into a behavior with no state.
func TestDurableActorRestartFailedTerminates(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}

	loadErr := errors.New("checkpoint store is wedged")
	store.injectCheckpointError = loadErr

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return false, 0
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
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for termination")
	}

	require.Equal(t, TerminationRestartFailed, info.Reason)
	require.False(t, info.RestartsExhausted)
	require.ErrorIs(t, info.Err, loadErr)
	require.Equal(t, 1, info.Restarts)

	require.NoError(t, a.Wait(context.Background()))
	require.Error(t, a.ctx.Err())
}

// TestDurableActorCleanupPanicTerminates verifies that a behavior whose OnStop
// panics during a restart is recovered rather than taking the process down,
// and is reported as a failed restart.
func TestDurableActorCleanupPanicTerminates(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}
	behavior.stopPanics.Store(true)

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return false, 0
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
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for termination")
	}

	require.Equal(t, TerminationRestartFailed, info.Reason)
	require.True(t, isBehaviorPanic(info.Err))

	// The hook panicked on the restart path and is not run a second time
	// by the terminal teardown, so one teardown means one OnStop.
	require.NoError(t, a.Wait(context.Background()))
	require.Equal(t, int32(1), behavior.stopCalls.Load())
}

// TestDurableActorConcurrentPanicsRecordOneRestart verifies that two workers
// panicking inside the SAME generation cost one unit of restart budget, not
// two. Charging per panicking worker would let a pool burn a finite budget
// N times faster than a single-worker actor for the same fault.
func TestDurableActorConcurrentPanicsRecordOneRestart(t *testing.T) {
	t.Parallel()

	const numWorkers = 4

	store := newMockTxAwareStore()
	behavior := &supervisedExecBehavior{}

	// Two of the four workers panic together: each parks on the barrier
	// until both have arrived, so both panics land in one generation.
	var barrier sync.WaitGroup
	barrier.Add(2)
	behavior.panicBarrier = &barrier
	behavior.barrierSize = 2

	cfg := DefaultDurableTxActorConfig[TLVMessage, int, DeliveryStore](
		"supervised-actor", behavior, identityStoreFactory, store,
		newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.NumWorkers = numWorkers
	cfg.MaxAttempts = 100
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return true, time.Millisecond
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	for i := 0; i < 2; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
		}
		require.NoError(t, a.Ref().Tell(context.Background(), msg))
	}

	// Both panics land, and the generation they shared is restarted once.
	require.Eventually(t, func() bool {
		return behavior.panics.Load() >= 2
	}, 10*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 10*time.Second, 10*time.Millisecond)

	require.Equal(t, 1, a.restarts.count())
}

// TestDurableActorPanicRollsBackBehaviorWrites verifies that the classic
// transactional path rolls the panicking turn's own writes back rather than
// committing them alongside the nack. The whole Receive runs inside one
// transaction there, so committing would persist exactly the torn state the
// restart exists to escape.
func TestDurableActorPanicRollsBackBehaviorWrites(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	behavior := &supervisedBehavior{
		shouldPanic: func(uint64) bool { return true },
	}

	cfg := DefaultDurableActorConfig[TLVMessage, int](
		"supervised-actor", behavior, store, newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return false, 0
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	// The panic reached ExecTx as the transaction's error, which is what
	// makes the store roll the behavior's writes back.
	require.Eventually(t, func() bool {
		return store.firstTxErr() != nil
	}, 5*time.Second, 10*time.Millisecond)

	require.True(t, isBehaviorPanic(store.firstTxErr()))

	// The bookkeeping still ran outside the rolled-back transaction, so
	// the poison message was dead-lettered rather than left in place.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		return len(store.deadLetters) == 1
	}, 5*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)
}

// TestDurableActorKeepsOneRestartRow verifies the restart row hygiene: a run
// of restarts leaves at most one restart message in the mailbox, and a restart
// turn that fails dead-letters rather than stranding a row that nothing will
// ever lease or reap again.
func TestDurableActorKeepsOneRestartRow(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()

	// The restore handler fails every time, which is the shape that piles
	// rows up: each restart enqueues one and the handler never consumes it
	// cleanly.
	behavior := &supervisedBehavior{
		shouldPanic:  func(uint64) bool { return true },
		failRestarts: true,
	}

	a := newSupervisedActor(
		t, store, behavior,
		func(cfg *DurableActorConfig[TLVMessage, int]) {
			cfg.MaxAttempts = 100
			cfg.TellRetryPolicy = func(error, int) (bool,
				time.Duration) {

				return true, time.Millisecond
			}
		},
	)

	a.Start()
	defer a.Stop()

	tellSupervised(t, a, 1)

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 3
	}, 10*time.Second, time.Millisecond)

	// Never more than one restart row pending at a time, and a failed
	// restart turn ends up in the dead letter queue rather than stranded
	// at attempts == max_attempts.
	store.mu.Lock()
	pending := 0
	for _, m := range store.messages {
		if m.MessageType == "actor.Restart" {
			pending++
		}
	}
	deadRestarts := 0
	for _, dl := range store.deadLetters {
		if dl.MessageType == "actor.Restart" {
			deadRestarts++
		}
	}
	store.mu.Unlock()

	require.LessOrEqual(t, pending, 1)
	require.Positive(t, deadRestarts)
}

// TestDurableActorWatchOnNeverStartedActor verifies that stopping an actor
// that was never started still publishes a termination, so a watcher on it
// does not wait forever for a supervision loop that will never run.
func TestDurableActorWatchOnNeverStartedActor(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	behavior := &supervisedBehavior{}

	a := newSupervisedActor(t, store, behavior, nil)

	watch := a.Watch(context.Background())
	a.Stop()

	select {
	case info := <-watch:
		require.Equal(t, TerminationStopped, info.Reason)
		require.Zero(t, info.Restarts)

	case <-time.After(5 * time.Second):
		t.Fatal("never-started actor published no termination")
	}

	_, ok := <-watch
	require.False(t, ok)

	// A watcher that registers afterwards is served from the recorded
	// notification just as it is for a started actor.
	late := <-a.Watch(context.Background())
	require.Equal(t, TerminationStopped, late.Reason)
}

// restoringExecBehavior models the shape every durable adopter in this repo
// actually has: a Read/Commit behavior holding in-memory state that mirrors a
// durable row, plus a reload guard it arms whenever that mirror might have run
// ahead of the row. credit.opBehavior and oor.sessionBehavior arm exactly this
// guard on a rolled-back Commit, and (since supervision landed) on a restart
// message too. It exists so the restart contract is tested against a behavior
// that can actually diverge, rather than one for which any handler would pass.
type restoringExecBehavior struct {
	mu sync.Mutex

	// store is the durable row this behavior mirrors.
	store *mockTxAwareStore

	// actorID keys the checkpoint that holds the durable value.
	actorID string

	// value is the in-memory mirror of the durable row: the analogue of
	// credit's rec or oor's fsm.
	value int64

	// commitFailed is the reload guard. When set, the next turn rebuilds
	// value from the durable row before it does anything else.
	commitFailed bool

	// observed records the value each observe turn saw AFTER any reload,
	// which is what the test asserts against.
	observed []int64

	// restarts counts the restart messages seen.
	restarts int
}

// restore rebuilds the in-memory value from the durable checkpoint.
func (b *restoringExecBehavior) restore(ctx context.Context) error {
	checkpoint, err := b.store.LoadCheckpoint(ctx, b.actorID)
	if err != nil {
		return err
	}

	if checkpoint == nil || len(checkpoint.StateData) != 8 {
		b.value = 0

		return nil
	}

	b.value = int64(binary.BigEndian.Uint64(checkpoint.StateData))

	return nil
}

// Receive implements TxBehavior. Value 1 advances the in-memory mirror past
// the durable row and then panics, which is the divergence a restart has to
// undo. Value 2 observes the mirror after any pending reload.
func (b *restoringExecBehavior) Receive(ctx context.Context, msg TLVMessage,
	ax Exec[DeliveryStore]) fn.Result[int] {

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := msg.(*RestartMessage); ok {
		b.restarts++

		// The seam: the framework reuses this instance across the
		// restart, so arm the reload rather than treating the message
		// as a no-op.
		b.commitFailed = true

		if err := ax.Commit(ctx, noOpCommit); err != nil {
			return fn.Err[int](err)
		}

		return fn.Ok(0)
	}

	test, ok := msg.(*actorTestMsg)
	if !ok {
		return fn.Err[int](errors.New("unexpected message type"))
	}

	if test.Value.Val == 1 {
		// Advance the mirror past the durable row, then die before
		// anything could persist it.
		b.value += 100

		panic("restoring exec behavior panic")
	}

	if b.commitFailed {
		if err := b.restore(ctx); err != nil {
			return fn.Err[int](err)
		}
		b.commitFailed = false
	}

	b.observed = append(b.observed, b.value)

	if err := ax.Commit(ctx, noOpCommit); err != nil {
		return fn.Err[int](err)
	}

	return fn.Ok(0)
}

// observations returns the values the observe turns saw.
func (b *restoringExecBehavior) observations() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]int64(nil), b.observed...)
}

// restartCount returns how many restart messages the behavior has seen.
func (b *restoringExecBehavior) restartCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.restarts
}

// TestDurableActorRestartReloadsDivergedState verifies the restart contract
// end to end against a behavior that can actually diverge. The behavior
// advances its in-memory mirror past the durable row and then panics, and the
// next message must see the durable value rather than the stale advance.
//
// This is the test that fails if a RestartMessage handler treats the message
// as a no-op, which is exactly what every Read/Commit adopter did before
// supervision existed: the framework reuses the behavior INSTANCE across a
// restart, so the reload has to come from the handler.
func TestDurableActorRestartReloadsDivergedState(t *testing.T) {
	t.Parallel()

	const durableValue = int64(7)

	store := newMockTxAwareStore()

	// The durable row the behavior mirrors.
	var stateData [8]byte
	binary.BigEndian.PutUint64(stateData[:], uint64(durableValue))
	require.NoError(
		t,
		store.SaveCheckpoint(
			context.Background(), CheckpointParams{
				ActorID:   "supervised-actor",
				StateType: "MirrorState",
				StateData: stateData[:],
				Version:   1,
			},
		),
	)

	behavior := &restoringExecBehavior{
		store:   store,
		actorID: "supervised-actor",
		value:   durableValue,
	}

	cfg := DefaultDurableTxActorConfig[TLVMessage, int, DeliveryStore](
		"supervised-actor", behavior, identityStoreFactory, store,
		newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond

	// Give up on the poison message immediately so it does not keep
	// re-panicking behind the observation.
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return false, 0
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	// Diverge and die.
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), msg))

	require.Eventually(t, func() bool {
		return behavior.restartCount() >= 1
	}, 10*time.Second, 10*time.Millisecond)

	// Observe after the restart.
	observe := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(2)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), observe))

	require.Eventually(t, func() bool {
		return len(behavior.observations()) >= 1
	}, 10*time.Second, 10*time.Millisecond)

	// The stale in-memory advance did not survive the restart: the turn
	// after it saw durable truth. Without the handler arming its reload
	// guard this would be durableValue + 100.
	require.Equal(t, []int64{durableValue}, behavior.observations())
}

// gatedExecBehavior is a Read/Commit behavior whose restart handler parks on a
// gate, so a test can hold a generation's checkpoint hand-off open and watch
// what the rest of a worker pool does meanwhile.
type gatedExecBehavior struct {
	// gate releases the restore turn when closed.
	gate chan struct{}

	// entered closes when a restore turn has begun, which is the moment
	// the warm-up barrier is provably holding the pool.
	entered   chan struct{}
	enterOnce sync.Once

	// normals counts committed non-restart turns. Nothing may increment it
	// while the gate is shut.
	normals atomic.Int32

	// panics makes the first delivery of value 1 panic, which is how the
	// test provokes the supervised restart it wants to observe.
	panics atomic.Int32

	// failRestore makes the restore turn fail rather than park, which must
	// still release the pool.
	failRestore bool
}

// Receive implements TxBehavior over the generic TLVMessage type.
func (b *gatedExecBehavior) Receive(ctx context.Context, msg TLVMessage,
	ax Exec[DeliveryStore]) fn.Result[int] {

	if _, ok := msg.(*RestartMessage); ok {
		b.enterOnce.Do(func() { close(b.entered) })

		if b.failRestore {
			return fn.Err[int](errors.New("restore failed"))
		}

		// Park until the test opens the gate. The context arm is what
		// lets a Stop landing mid-barrier unwedge this turn.
		select {
		case <-b.gate:
		case <-ctx.Done():
			return fn.Err[int](ctx.Err())
		}

		if err := ax.Commit(ctx, noOpCommit); err != nil {
			return fn.Err[int](err)
		}

		return fn.Ok(0)
	}

	test, ok := msg.(*actorTestMsg)
	if !ok {
		return fn.Err[int](errors.New("unexpected message type"))
	}

	if test.Value.Val == 1 && b.panics.Add(1) == 1 {
		panic("gated exec behavior panic")
	}

	if err := ax.Commit(ctx, noOpCommit); err != nil {
		return fn.Err[int](err)
	}

	// Only the backlog counts. The message that provoked the restart is
	// redelivered afterwards and commits harmlessly, but counting it would
	// make the backlog assertions read as an off-by-one rather than as the
	// ordering property they are about.
	if test.Value.Val != 1 {
		b.normals.Add(1)
	}

	return fn.Ok(0)
}

// newGatedPoolActor builds a competing-consumer pool over a gatedExecBehavior.
func newGatedPoolActor(t *testing.T, store *mockTxAwareStore,
	behavior *gatedExecBehavior,
	numWorkers int) *DurableActor[TLVMessage, int] {

	t.Helper()

	cfg := DefaultDurableTxActorConfig[TLVMessage, int, DeliveryStore](
		"supervised-actor", behavior, identityStoreFactory, store,
		newSupervisedCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxPollInterval = 10 * time.Millisecond
	cfg.NumWorkers = numWorkers
	cfg.MaxAttempts = 100
	cfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return true, time.Millisecond
	}

	return NewDurableActor(cfg).UnwrapOrFail(t)
}

// newGatedBehavior builds a gated behavior with its channels wired.
func newGatedBehavior() *gatedExecBehavior {
	return &gatedExecBehavior{
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
}

// TestDurableActorPoolWarmupBarrierHoldsRestart verifies the ordering
// guarantee under a competing-consumer pool: while a restart hand-off is being
// processed, no sibling worker may run a normal turn against the same behavior
// instance.
//
// RestartPriority orders the CLAIMS, not the turns. Launching a pool all at
// once lets one worker take the restart message while a sibling immediately
// takes the row behind it, so a normal turn runs against a behavior that is
// still rebuilding itself from the checkpoint. The warm-up barrier is what
// turns the documented "processed before all other messages" into something
// that actually holds.
func TestDurableActorPoolWarmupBarrierHoldsRestart(t *testing.T) {
	t.Parallel()

	const (
		numWorkers = 4
		backlog    = 6
	)

	store := newMockTxAwareStore()
	behavior := newGatedBehavior()

	a := newGatedPoolActor(t, store, behavior, numWorkers)
	a.Start()
	defer a.Stop()

	// Provoke a supervised restart. The framework enqueues the restart
	// message itself, so the barrier holds for it unconditionally rather
	// than inferring it from the first claim.
	panicMsg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), panicMsg))

	// Wait until the restore turn is parked in the gate. From here on the
	// barrier is provably shut and only the warm-up worker exists.
	select {
	case <-behavior.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("restore turn never started")
	}

	// Queue a backlog behind the parked restore. Every one of these is
	// claim-eligible, so a pool that had fanned out would drain them.
	for i := 0; i < backlog; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(2)),
		}
		require.NoError(t, a.Ref().Tell(context.Background(), msg))
	}

	// Nothing may run while the hand-off is open. The window is many poll
	// intervals wide, so a pool that fanned out early would be caught.
	require.Never(t, func() bool {
		return behavior.normals.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Release the restore and the pool fans out to drain the backlog.
	close(behavior.gate)

	require.Eventually(t, func() bool {
		return behavior.normals.Load() == int32(backlog)
	}, 10*time.Second, 10*time.Millisecond)

	require.NoError(t, a.ctx.Err())
}

// TestDurableActorPoolWarmupBarrierReleasesOnFailedRestore verifies the
// barrier cannot wedge a pool when the restore turn fails. A failed restart
// turn is dead-lettered rather than retried, so the hand-off is resolved and
// the pool must fan out.
func TestDurableActorPoolWarmupBarrierReleasesOnFailedRestore(t *testing.T) {
	t.Parallel()

	const (
		numWorkers = 4
		backlog    = 6
	)

	store := newMockTxAwareStore()
	behavior := newGatedBehavior()
	behavior.failRestore = true

	a := newGatedPoolActor(t, store, behavior, numWorkers)
	a.Start()
	defer a.Stop()

	panicMsg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), panicMsg))

	select {
	case <-behavior.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("restore turn never started")
	}

	for i := 0; i < backlog; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(2)),
		}
		require.NoError(t, a.Ref().Tell(context.Background(), msg))
	}

	// The restore failed, so the barrier releases and the backlog drains.
	require.Eventually(t, func() bool {
		return behavior.normals.Load() == int32(backlog)
	}, 10*time.Second, 10*time.Millisecond)

	// The failed restart went to the dead letter queue rather than being
	// retried into a second barrier.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		for _, dl := range store.deadLetters {
			if dl.MessageType == "actor.Restart" {
				return true
			}
		}

		return false
	}, 10*time.Second, 10*time.Millisecond)
}

// TestDurableActorPoolWarmupBarrierStopUnblocks verifies that a Stop landing
// while the barrier is shut terminates the actor cleanly rather than parking
// shutdown behind a restore that will never finish.
func TestDurableActorPoolWarmupBarrierStopUnblocks(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	behavior := newGatedBehavior()

	// The gate is never opened: only the generation context can end the
	// restore turn.
	a := newGatedPoolActor(t, store, behavior, 4)

	watch := a.Watch(context.Background())

	a.Start()

	panicMsg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), panicMsg))

	select {
	case <-behavior.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("restore turn never started")
	}

	stopped := make(chan error, 1)
	go func() {
		stopCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()

		stopped <- a.StopAndWait(stopCtx)
	}()

	select {
	case err := <-stopped:
		require.NoError(t, err)

	case <-time.After(10 * time.Second):
		t.Fatal("shutdown parked behind the warm-up barrier")
	}

	info := <-watch
	require.Equal(t, TerminationStopped, info.Reason)
}

// TestDurableActorPoolWithoutRestartFansOut verifies the barrier costs a pool
// nothing when there is no hand-off to order: the first claim of a normal
// message releases it before that message is even processed, so the pool is at
// full width for the work behind it.
func TestDurableActorPoolWithoutRestartFansOut(t *testing.T) {
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
	cfg.MaxAttempts = 100

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	// Every worker parks inside its turn, so reaching numWorkers parked
	// turns is only possible once the whole pool is running.
	for i := 0; i < numWorkers; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(0)),
		}
		require.NoError(t, a.Ref().Tell(context.Background(), msg))
	}

	require.Eventually(t, func() bool {
		return behavior.parked.Load() == numWorkers
	}, 10*time.Second, 10*time.Millisecond)
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
// OFF: a finite budget kills the actor permanently, so both the default config
// and a hand-built config that never mentions MaxRestarts must land on
// unlimited rather than inheriting a kill switch. A finite budget is honored
// only when it is asked for.
func TestDurableActorRestartBudgetDefaults(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newSupervisedCodec()
	behavior := &supervisedBehavior{}

	require.Equal(t, UnlimitedRestarts, DefaultMaxRestarts)

	cfg := DefaultDurableActorConfig[TLVMessage, int](
		"a", behavior, store, codec,
	)
	require.Equal(t, UnlimitedRestarts, cfg.MaxRestarts)
	require.Equal(t, DefaultRestartWindow, cfg.RestartWindow)

	defaulted := NewDurableActor(cfg).UnwrapOrFail(t)
	require.Equal(t, UnlimitedRestarts, defaulted.restarts.max)

	// A hand-built config with a zero MaxRestarts normalizes to unlimited
	// too, so a config that predates supervision cannot acquire a silent
	// kill switch by omission.
	bare := DurableActorConfig[TLVMessage, int]{
		ID:       "a",
		Behavior: NewClassicBehavior[TLVMessage, int](behavior),
		Store:    store,
		Codec:    codec,
	}
	bareActor := NewDurableActor(bare).UnwrapOrFail(t)
	require.Equal(t, UnlimitedRestarts, bareActor.restarts.max)
	require.Equal(t, DefaultRestartWindow, bareActor.restarts.window)

	// An explicitly chosen finite budget is honored verbatim.
	cfg.MaxRestarts = RecommendedMaxRestarts
	finite := NewDurableActor(cfg).UnwrapOrFail(t)
	require.Equal(t, RecommendedMaxRestarts, finite.restarts.max)
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
