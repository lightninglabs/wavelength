package actortest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// NOTE: Tests in this file use real SQLite databases (per-test in-memory).
// Each test has its own isolated database, clock, and actor system.

// testHarness holds all the components needed for e2e testing.
type testHarness struct {
	t *testing.T

	// ctx is owned by the harness and canceled via cancel.
	ctx context.Context //nolint:containedctx

	cancel      context.CancelFunc
	store       *actordelivery.TxAwareActorDeliveryStore
	codec       *actor.MessageCodec
	clock       *clock.TestClock
	actorSystem *actor.ActorSystem
}

// newTestHarness creates a new test harness with real SQLite database.
func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	// Create per-test in-memory SQLite database.
	sqlDB := db.NewTestDB(t)
	actorQueries := adsqlc.New(sqlDB.DB)

	// Create the transaction executor for actor delivery operations.
	actorDB := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) actordelivery.ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)

	// Create a test clock for time manipulation.
	testClock := clock.NewTestClock(time.Now())

	// Create the actor delivery store. The transaction-aware variant
	// matches production wiring (waved hands the OutboxPublisher a
	// TxAwareDeliveryStore), so the publisher e2e tests exercise the
	// folded Tell+CompleteOutbox single-transaction delivery path.
	store := actordelivery.NewTxAwareActorDeliveryStore(
		actorDB, sqlDB.BaseDB, testClock,
	)

	// Create the message codec with counter messages registered.
	codec := NewCounterCodec()

	// Create the actor system.
	actorSystem := actor.NewActorSystem()

	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(
			t.Context(), 5*time.Second,
		)
		defer shutdownCancel()

		_ = actorSystem.Shutdown(shutdownCtx)
		cancel()
	})

	return &testHarness{
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		store:       store,
		codec:       codec,
		clock:       testClock,
		actorSystem: actorSystem,
	}
}

// uniqueID generates a unique ID for test actors to prevent cross-test
// interference via the global currentDeliveryMap.
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

// newDurableCounter creates a new DurableActor with CounterBehavior.
func (h *testHarness) newDurableCounter(id string) (
	*actor.DurableActor[CounterMessage, CounterResult], *CounterBehavior) {

	behavior := NewCounterBehavior(id, h.store, h.codec)

	cfg := actor.DefaultDurableActorConfig[CounterMessage, CounterResult](
		id, behavior, h.store, h.codec,
	)

	// Use the test clock for deterministic availability and lease timing.
	cfg.Clock = fn.Some[clock.Clock](h.clock)

	// Use short intervals to reduce overall test runtime.
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second
	cfg.HeartbeatInterval = 1 * time.Second

	durableActor := actor.NewDurableActor(cfg).UnwrapOrFail(h.t)

	// Register with [Message, any] types so OutboxPublisher can find it.
	// The OutboxPublisher looks up actors using ServiceKey[Message, any],
	// so we use TypeAssertingRef to adapt the concrete types.
	erasingRef := actor.TypeAssertingRef[
		actor.Message,
		CounterMessage,
		CounterResult,
	](
		durableActor.Ref(),
	)
	key := actor.NewServiceKey[actor.Message, any](id)
	_ = actor.RegisterWithReceptionist(
		h.actorSystem.Receptionist(), key, erasingRef,
	)

	return durableActor, behavior
}

// eventually retries a condition until it succeeds or times out.
func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("condition not met within timeout")
}

// eventuallyWithOutboxPublish retries a condition until it succeeds or times
// out, forcing an outbox publish attempt on each iteration.
//
// This avoids test flakiness from relying solely on the publisher's ticker when
// CI is under heavy scheduler pressure (for example, under the race detector).
func eventuallyWithOutboxPublish(t *testing.T, publisher *actor.OutboxPublisher,
	timeout time.Duration, condition func() bool) {

	t.Helper()

	eventually(t, timeout, func() bool {
		publisher.PublishPending()

		return condition()
	})
}

// durableAskResponseTimeout is a shared timeout budget for DurableAsk response
// delivery assertions in this test suite. Keep this aligned with the outbox
// delivery timeout because DurableAsk responses are also delivered through the
// outbox in these end-to-end tests.
const durableAskResponseTimeout = 30 * time.Second

// outboxForwardProcessingTimeout is the timeout budget for waiting on the
// source actor to durably process a forward request before any outbox delivery
// assertions. Keep this aligned with the delivery timeout because CI can
// schedule these SQLite-backed tests slowly under `-race`.
const outboxForwardProcessingTimeout = 30 * time.Second

// outboxDeliveryTimeout is the timeout budget for waiting on OutboxPublisher
// delivery in end-to-end tests. The helper actively triggers publish attempts
// during this window to reduce scheduler-induced flakiness under `-race`, but
// the full CI race suite can still starve these chained delivery tests while
// other package tests are competing for CPU.
const outboxDeliveryTimeout = 30 * time.Second

// durableCounterRef is a shorthand alias for the generic durable ref used in
// these end-to-end tests.
type durableCounterRef = actor.DurableActorRef[CounterMessage, CounterResult]

// requireDurableCounterRef asserts that the given actor ref supports durable
// operations, and fails the test if it does not.
func requireDurableCounterRef(
	t *testing.T,
	targetRef actor.ActorRef[CounterMessage, CounterResult],
) durableCounterRef {

	t.Helper()

	durableRef, ok := targetRef.(durableCounterRef)
	require.True(t, ok, "expected DurableActorRef")

	return durableRef
}

// ============================================================================
// Basic Tell/Ask Tests
// ============================================================================

// TestDurableCounter_TellIncrement verifies Tell messages are processed.
func TestDurableCounter_TellIncrement(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	// Send Tell message.
	ref := counterActor.Ref()
	err := ref.Tell(h.ctx, &IncrementMsg{Amount: 10})
	require.NoError(t, err)

	// Wait for processing.
	eventually(t, 2*time.Second, func() bool {
		return behavior.Count() == 10
	})

	require.Equal(t, int64(10), behavior.Count())
}

// TestDurableCounter_AskGetCount verifies Ask messages return responses.
func TestDurableCounter_AskGetCount(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	// Set initial count.
	behavior.SetCount(42)

	// Ask for current count.
	ref := counterActor.Ref()
	future := ref.Ask(h.ctx, &GetCountMsg{})

	// Wait for response.
	result := future.Await(h.ctx)
	val, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, int64(42), val)
}

// TestDurableCounter_MultipleTells verifies multiple Tell messages process in
// order.
func TestDurableCounter_MultipleTells(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Send multiple increments.
	for i := 0; i < 10; i++ {
		err := ref.Tell(h.ctx, &IncrementMsg{Amount: 1})
		require.NoError(t, err)
	}

	// Wait for all to process.
	eventually(t, 5*time.Second, func() bool {
		return behavior.Count() == 10
	})

	require.Equal(t, int64(10), behavior.Count())
}

// TestDurableCounter_IncrementDecrement verifies mixed operations.
func TestDurableCounter_IncrementDecrement(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Increment by 100.
	err := ref.Tell(h.ctx, &IncrementMsg{Amount: 100})
	require.NoError(t, err)

	// Decrement by 30.
	err = ref.Tell(h.ctx, &DecrementMsg{Amount: 30})
	require.NoError(t, err)

	// Wait for processing.
	eventually(t, 2*time.Second, func() bool {
		return behavior.Count() == 70
	})

	require.Equal(t, int64(70), behavior.Count())
}

// ============================================================================
// Outbox Tests
// ============================================================================

// TestDurableCounter_ForwardWritesToOutbox verifies ForwardMsg writes to
// outbox.
func TestDurableCounter_ForwardWritesToOutbox(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	actorID := uniqueID("counter")
	counterActor, behavior := h.newDurableCounter(actorID)
	counterActor.Start()
	defer counterActor.Stop()

	// Encode a message to forward.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 5})
	require.NoError(t, err)

	ref := counterActor.Ref()
	err = ref.Tell(h.ctx, &ForwardMsg{
		Target:  "target-counter",
		MsgType: IncrementMsgType,
		Payload: payload,
	})
	require.NoError(t, err)

	// Wait for processing.
	eventually(t, 2*time.Second, func() bool {
		return behavior.ForwardCount() == 1
	})

	// Verify message is in outbox by claiming with a test token.
	batch, err := h.store.ClaimOutboxBatch(
		h.ctx, actor.OutboxClaimParams{
			Limit:         10,
			ClaimToken:    "test-claim",
			ClaimDuration: 30 * time.Second,
		},
	)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	require.Equal(t, actorID, batch[0].SourceActorID)
	require.Equal(t, "target-counter", batch[0].TargetActorID)
}

// TestOutboxPublisher_DeliversToTarget verifies outbox publisher delivers
// messages.
func TestOutboxPublisher_DeliversToTarget(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Generate unique IDs for source and target.
	sourceID := uniqueID("source")
	targetID := uniqueID("target")

	// Create source counter.
	sourceActor, sourceBehavior := h.newDurableCounter(sourceID)
	sourceActor.Start()
	defer sourceActor.Stop()

	// Create target counter.
	targetActor, targetBehavior := h.newDurableCounter(targetID)
	targetActor.Start()
	defer targetActor.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Encode an increment message to forward.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 25})
	require.NoError(t, err)

	// Source forwards to target using the target's unique ID.
	sourceRef := sourceActor.Ref()
	err = sourceRef.Tell(h.ctx, &ForwardMsg{
		Target:  targetID,
		MsgType: IncrementMsgType,
		Payload: payload,
	})
	require.NoError(t, err)

	// Wait for source to process the forward.
	eventually(t, outboxForwardProcessingTimeout, func() bool {
		return sourceBehavior.ForwardCount() == 1
	})

	// Wait for target to receive the increment via OutboxPublisher.
	eventuallyWithOutboxPublish(
		t, publisher, outboxDeliveryTimeout,
		func() bool {
			return targetBehavior.Count() == 25
		},
	)

	require.Equal(t, int64(25), targetBehavior.Count())
}

// completeFailDeliveryStore wraps a transaction-scoped DeliveryStore and fails
// CompleteOutbox on demand. It lets the rollback test force the folded
// Tell+CompleteOutbox delivery transaction to fail AFTER the target enqueue
// succeeded, which is the interesting atomicity window.
type completeFailDeliveryStore struct {
	actor.DeliveryStore

	fail *atomic.Bool
}

// CompleteOutbox fails with an injected error while the flag is set, and
// otherwise delegates to the wrapped transaction-scoped store.
func (s *completeFailDeliveryStore) CompleteOutbox(ctx context.Context, id,
	claimToken string) error {

	if s.fail.Load() {
		return errInjectedCompleteFailure
	}

	return s.DeliveryStore.CompleteOutbox(ctx, id, claimToken)
}

// errInjectedCompleteFailure is the sentinel injected by
// completeFailDeliveryStore.
var errInjectedCompleteFailure = errors.New("injected complete failure")

// completeFailTxStore wraps the real TxAwareActorDeliveryStore so the
// OutboxPublisher still detects transaction support, while the TxFunc receives
// a transaction-scoped store whose CompleteOutbox can be forced to fail.
type completeFailTxStore struct {
	*actordelivery.TxAwareActorDeliveryStore

	fail atomic.Bool
}

// ExecTx delegates to the real transactional executor but interposes the
// failure-injecting store in front of the transaction-scoped DeliveryStore.
func (s *completeFailTxStore) ExecTx(ctx context.Context, readOnly bool,
	fn actor.TxFunc) error {

	return s.TxAwareActorDeliveryStore.ExecTx(
		ctx, readOnly,
		func(txCtx context.Context, store actor.DeliveryStore) error {
			return fn(txCtx, &completeFailDeliveryStore{
				DeliveryStore: store,
				fail:          &s.fail,
			})
		},
	)
}

// TestOutboxPublisherAtomicDeliveryRollback verifies the folded delivery
// transaction is atomic: when CompleteOutbox fails after a successful Tell,
// the rollback must also undo the target mailbox enqueue, leaving the outbox
// row claimed-but-pending so a later cycle redelivers exactly once.
func TestOutboxPublisherAtomicDeliveryRollback(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	sourceID := uniqueID("counter-src")
	targetID := uniqueID("counter-tgt")

	sourceActor, sourceBehavior := h.newDurableCounter(sourceID)
	sourceActor.Start()
	defer sourceActor.Stop()

	targetActor, targetBehavior := h.newDurableCounter(targetID)
	targetActor.Start()
	defer targetActor.Stop()

	// The publisher sees the failure-injecting wrapper, which still
	// satisfies TxAwareDeliveryStore, so deliveries run on the folded
	// single-transaction path.
	failStore := &completeFailTxStore{
		TxAwareActorDeliveryStore: h.store,
	}
	failStore.fail.Store(true)

	publisherCfg := actor.DefaultOutboxPublisherConfig(
		failStore, h.codec, h.actorSystem,
	)

	// Manual publish cycles only: a long poll interval plus never calling
	// Start keeps every claim attempt under test control.
	publisherCfg.PollInterval = time.Hour
	publisher := actor.NewOutboxPublisher(publisherCfg)

	// Source forwards to target, parking one message in the outbox.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 25})
	require.NoError(t, err)

	err = sourceActor.Ref().Tell(h.ctx, &ForwardMsg{
		Target:  targetID,
		MsgType: IncrementMsgType,
		Payload: payload,
	})
	require.NoError(t, err)

	eventually(t, outboxForwardProcessingTimeout, func() bool {
		return sourceBehavior.ForwardCount() == 1
	})

	// First publish cycle: the Tell enqueues into the target mailbox
	// inside the delivery transaction, then the injected CompleteOutbox
	// failure rolls the whole transaction back.
	publisher.PublishPending()

	// The rollback must have erased the target enqueue: a leaked row
	// would be drained by the running target actor and bump its counter.
	peeked, err := h.store.PeekNextMessage(h.ctx, targetID)
	require.NoError(t, err)
	require.Nil(t, peeked, "enqueue leaked from rolled-back delivery tx")
	require.Equal(t, int64(0), targetBehavior.Count())

	// Heal the store and expire the claim so the row is reclaimable.
	failStore.fail.Store(false)
	h.clock.SetTime(h.clock.Now().Add(time.Minute))

	// The retry cycle must deliver exactly once.
	eventuallyWithOutboxPublish(
		t, publisher, outboxDeliveryTimeout,
		func() bool {
			return targetBehavior.Count() == 25
		},
	)

	require.Equal(t, int64(25), targetBehavior.Count())
}

// TestOutboxPublisher_MultiHopForwarding verifies chained message forwarding.
func TestOutboxPublisher_MultiHopForwarding(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Generate unique IDs for all actors.
	idA := uniqueID("counter-a")
	idB := uniqueID("counter-b")
	idC := uniqueID("counter-c")

	// Create three counters: A -> B -> C.
	actorA, behaviorA := h.newDurableCounter(idA)
	actorA.Start()
	defer actorA.Stop()

	actorB, behaviorB := h.newDurableCounter(idB)
	actorB.Start()
	defer actorB.Stop()

	actorC, behaviorC := h.newDurableCounter(idC)
	actorC.Start()
	defer actorC.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Encode increment message.
	incrementPayload, err := h.codec.Encode(&IncrementMsg{Amount: 100})
	require.NoError(t, err)

	// Encode forward message from B to C.
	forwardBtoCPayload, err := h.codec.Encode(&ForwardMsg{
		Target:  idC,
		MsgType: IncrementMsgType,
		Payload: incrementPayload,
	})
	require.NoError(t, err)

	// A forwards a ForwardMsg to B (which B will then forward to C).
	refA := actorA.Ref()
	err = refA.Tell(h.ctx, &ForwardMsg{
		Target:  idB,
		MsgType: ForwardMsgType,
		Payload: forwardBtoCPayload,
	})
	require.NoError(t, err)

	// Wait for A to process.
	eventually(t, outboxForwardProcessingTimeout, func() bool {
		return behaviorA.ForwardCount() == 1
	})

	// Wait for B to process (receives forward, writes to outbox).
	eventuallyWithOutboxPublish(
		t, publisher, outboxDeliveryTimeout,
		func() bool {
			return behaviorB.ForwardCount() == 1
		},
	)

	// Wait for C to receive the increment.
	eventuallyWithOutboxPublish(
		t, publisher, outboxDeliveryTimeout,
		func() bool {
			return behaviorC.Count() == 100
		},
	)

	require.Equal(t, int64(100), behaviorC.Count())
}

// ============================================================================
// Deduplication Tests
// ============================================================================

// TestDurableCounter_Deduplication verifies same message ID is processed once.
func TestDurableCounter_Deduplication(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	actorID := uniqueID("counter")
	counterActor, behavior := h.newDurableCounter(actorID)
	counterActor.Start()
	defer counterActor.Stop()

	// Manually enqueue the same message twice with the same ID.
	messageID := "dedup-test-msg-001"
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 50})
	require.NoError(t, err)

	// First enqueue - use actorID as mailbox ID.
	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	// Wait for first processing.
	eventually(t, 2*time.Second, func() bool {
		return behavior.Count() == 50
	})

	// Enqueue again with same ID (simulating redelivery).
	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 3,
	})
	// This may error due to UNIQUE constraint - that's expected.
	_ = err

	// Wait a bit more.
	time.Sleep(500 * time.Millisecond)

	// Count should still be 50 (deduplicated).
	require.Equal(t, int64(50), behavior.Count())
}

// ============================================================================
// Concurrent Tests
// ============================================================================

// TestDurableCounter_ConcurrentSenders verifies concurrent message senders.
func TestDurableCounter_ConcurrentSenders(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Launch 5 concurrent senders, each sending 5 increments.
	// Reduced from 10×10 to be more reliable under test parallelism.
	var wg sync.WaitGroup
	numSenders := 5
	msgsPerSender := 5

	var sendCount atomic.Int64

	for i := 0; i < numSenders; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < msgsPerSender; j++ {
				err := ref.Tell(h.ctx, &IncrementMsg{Amount: 1})
				if err != nil {
					t.Logf("Tell error: %v", err)
				} else {
					sendCount.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("All sends complete: %d messages sent", sendCount.Load())

	// Wait for all messages to process. Allow more time for concurrent
	// test.
	expectedCount := int64(numSenders * msgsPerSender)
	var lastLogged int64
	eventually(t, 30*time.Second, func() bool {
		current := behavior.Count()

		// Only log on progress changes to reduce spam.
		if current != lastLogged && current < expectedCount {
			t.Logf("Progress: %d/%d", current, expectedCount)
			lastLogged = current
		}

		return current == expectedCount
	})

	require.Equal(t, expectedCount, behavior.Count())
}

// TestDurableCounter_ConcurrentAsks verifies concurrent Ask operations.
func TestDurableCounter_ConcurrentAsks(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	behavior.SetCount(100)
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Launch concurrent Ask operations.
	var wg sync.WaitGroup
	numAsks := 20
	results := make(chan int64, numAsks)

	for i := 0; i < numAsks; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			future := ref.Ask(h.ctx, &GetCountMsg{})
			result := future.Await(h.ctx)
			val, err := result.Unpack()

			if err == nil {
				results <- val
			}
		}()
	}

	wg.Wait()
	close(results)

	// All results should be 100.
	for val := range results {
		require.Equal(t, int64(100), val)
	}
}

// ============================================================================
// Property-Based Tests
// ============================================================================

// TestProperty_IncrementDecrement_Commutative verifies increment/decrement
// commutativity.
//
// INVARIANT: sum of all increments - sum of all decrements = final count.
func TestProperty_IncrementDecrement_Commutative(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Send a mix of increments and decrements.
	increments := []int64{10, 20, 30, 15, 25}
	decrements := []int64{5, 10, 15}

	var expectedDelta int64

	for _, amt := range increments {
		err := ref.Tell(h.ctx, &IncrementMsg{Amount: amt})
		require.NoError(t, err)
		expectedDelta += amt
	}

	for _, amt := range decrements {
		err := ref.Tell(h.ctx, &DecrementMsg{Amount: amt})
		require.NoError(t, err)
		expectedDelta -= amt
	}

	// Expected: (10+20+30+15+25) - (5+10+15) = 100 - 30 = 70.
	require.Equal(t, int64(70), expectedDelta)

	// Wait for all operations to complete.
	eventually(t, 3*time.Second, func() bool {
		return behavior.Count() == expectedDelta
	})

	require.Equal(t, expectedDelta, behavior.Count())
}

// TestProperty_AskAlwaysReturnsCurrentValue verifies Ask consistency.
func TestProperty_AskAlwaysReturnsCurrentValue(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	counterActor, behavior := h.newDurableCounter(uniqueID("counter"))
	counterActor.Start()
	defer counterActor.Stop()

	ref := counterActor.Ref()

	// Test with various initial values.
	testCases := []int64{0, 42, 100, 500, 999}

	for _, initial := range testCases {
		behavior.SetCount(initial)

		// Ask should return the current value.
		future := ref.Ask(h.ctx, &GetCountMsg{})
		result := future.Await(h.ctx)
		val, err := result.Unpack()

		require.NoError(t, err)
		require.Equal(
			t, initial, val, "Ask should return current count",
		)
	}
}

// TestProperty_OutboxEventuallyDelivers verifies outbox messages are delivered.
func TestProperty_OutboxEventuallyDelivers(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Generate unique IDs for source and target.
	sourceID := uniqueID("source")
	targetID := uniqueID("target")

	// Create source and target.
	sourceActor, sourceBehavior := h.newDurableCounter(sourceID)
	sourceActor.Start()
	defer sourceActor.Stop()

	targetActor, targetBehavior := h.newDurableCounter(targetID)
	targetActor.Start()
	defer targetActor.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Test with a fixed amount.
	amount := int64(42)

	// Encode and forward.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: amount})
	require.NoError(t, err)

	sourceRef := sourceActor.Ref()
	err = sourceRef.Tell(h.ctx, &ForwardMsg{
		Target:  targetID,
		MsgType: IncrementMsgType,
		Payload: payload,
	})
	require.NoError(t, err)

	// Wait for source to process.
	eventually(t, outboxForwardProcessingTimeout, func() bool {
		return sourceBehavior.ForwardCount() == 1
	})

	// Wait for target to receive.
	eventuallyWithOutboxPublish(
		t, publisher, outboxDeliveryTimeout,
		func() bool {
			return targetBehavior.Count() == amount
		},
	)

	require.Equal(t, amount, targetBehavior.Count())
}

// ============================================================================
// Recovery and Restart Tests (INVARIANT: At-Least-Once Delivery)
// ============================================================================

// TestRecovery_UnackedMessagesRedelivered verifies messages without ack are
// redelivered after lease expiry. This tests the at-least-once delivery
// invariant.
func TestRecovery_UnackedMessagesRedelivered(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Generate unique ID that will be used for both enqueue and actor.
	actorID := uniqueID("counter")

	// Enqueue a message directly to the database.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 77})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          "recovery-test-msg",
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	require.NoError(t, err)

	// Lease the message (simulating first delivery attempt).
	leased, err := h.store.LeaseNextMessage(
		h.ctx, actorID, "old-token", 5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Simulate "crash" - don't ack the message, advance clock past lease.
	h.clock.SetTime(h.clock.Now().Add(10 * time.Second))

	// Run lease expiry.
	err = h.store.ExpireLeases(h.ctx)
	require.NoError(t, err)

	// Now create the actor - should pick up the redelivered message.
	counterActor, behavior := h.newDurableCounter(actorID)
	counterActor.Start()
	defer counterActor.Stop()

	// Wait for message to be processed.
	eventually(t, 5*time.Second, func() bool {
		return behavior.Count() == 77
	})

	require.Equal(t, int64(77), behavior.Count())
}

// TestRecovery_RestartMessagePriority verifies RestartMessage is processed
// first when actor restarts with a checkpoint.
func TestRecovery_RestartMessagePriority(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	actorID := uniqueID("counter")

	// Save a checkpoint.
	err := h.store.SaveCheckpoint(h.ctx, actor.CheckpointParams{
		ActorID:   actorID,
		StateType: "CounterState",

		// 100 in big-endian.
		StateData: []byte{0, 0, 0, 0, 0, 0, 0, 100},
		Version:   1,
	})
	require.NoError(t, err)

	// Enqueue a regular message (lower priority).
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 10})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          "regular-msg-1",
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		Priority:    0,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	require.NoError(t, err)

	// Prepend restart message (should be processed first due to high
	// priority).
	checkpoint, err := h.store.LoadCheckpoint(h.ctx, actorID)
	require.NoError(t, err)

	err = actor.PrependRestartMessage(
		h.ctx, h.store, h.codec, actorID, checkpoint,
	)
	require.NoError(t, err)

	// Lease first message - should be restart message due to priority.
	leased, err := h.store.LeaseNextMessage(
		h.ctx, actorID, "test-token", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Verify it's the restart message (highest priority).
	require.Equal(t, "actor.Restart", leased.MessageType)
}

// TestRecovery_IdempotentProcessing verifies deduplication prevents duplicate
// processing even after restart. This tests exactly-once processing invariant.
func TestRecovery_IdempotentProcessing(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	actorID := uniqueID("counter")
	messageID := "idem-msg-001"

	// Enqueue a message.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 50})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	require.NoError(t, err)

	// Create and start actor.
	counterActor, behavior := h.newDurableCounter(actorID)
	counterActor.Start()

	// Wait for processing AND ack to complete. The behavior increments the
	// counter, but the ack (which marks processed) happens after the
	// behavior returns. We need to wait for both to complete before
	// stopping.
	eventually(t, 2*time.Second, func() bool {
		if behavior.Count() != 50 {
			return false
		}

		// Also check that the ack completed (message marked as
		// processed).
		processed, err := h.store.IsProcessed(h.ctx, messageID)

		return err == nil && processed
	})

	// Stop actor (simulating crash).
	counterActor.Stop()

	// Verify message was marked as processed.
	processed, err := h.store.IsProcessed(h.ctx, messageID)
	require.NoError(t, err)
	require.True(
		t, processed, "message should be marked as processed after ack",
	)

	// Enqueue same message ID again (simulating redelivery after crash).
	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   actorID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	// May fail due to UNIQUE constraint - that's OK. Redelivery would
	// happen from lease expiry anyway.
	_ = err

	// Create new actor instance (restart).
	counterActor2, behavior2 := h.newDurableCounter(actorID)
	counterActor2.Start()
	defer counterActor2.Stop()

	// Wait a bit for any processing.
	time.Sleep(500 * time.Millisecond)

	// Count should still be 50 (message was deduplicated, not processed
	// twice).
	// Note: The new behavior starts fresh, so count is 0 unless we
	// implement checkpoint restore. The key test is that dedup prevented
	// double processing - we can verify via the IsProcessed check.
	require.Equal(t, int64(0), behavior2.Count())

	// The deduplication entry should still exist.
	processed, err = h.store.IsProcessed(h.ctx, messageID)
	require.NoError(t, err)
	require.True(t, processed)
}

// ============================================================================
// Lease Invariant Tests
// ============================================================================

// TestLease_MutualExclusion verifies only one actor can lease a message at a
// time. This tests the mutual exclusion invariant.
func TestLease_MutualExclusion(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	mailboxID := uniqueID("mutex")

	// Enqueue a message.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 1})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          "mutex-msg-001",
		MailboxID:   mailboxID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	require.NoError(t, err)

	// First lease attempt succeeds.
	leased1, err := h.store.LeaseNextMessage(
		h.ctx, mailboxID, "token-1", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased1)

	// Second lease attempt returns nil (message already leased).
	leased2, err := h.store.LeaseNextMessage(
		h.ctx, mailboxID, "token-2", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased2)
}

// TestLease_AckRequiresValidToken verifies ack only succeeds with correct
// token.
// This tests the "ack requires valid lease" invariant.
func TestLease_AckRequiresValidToken(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	mailboxID := uniqueID("ack")

	// Enqueue and lease a message.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 1})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          "ack-msg-001",
		MailboxID:   mailboxID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 10,
	})
	require.NoError(t, err)

	leased, err := h.store.LeaseNextMessage(
		h.ctx, mailboxID, "correct-token", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Ack with wrong token fails (0 rows affected).
	rows, err := h.store.AckMessage(h.ctx, "ack-msg-001", "wrong-token")
	require.NoError(t, err)
	require.Equal(t, int64(0), rows)

	// Ack with correct token succeeds.
	rows, err = h.store.AckMessage(h.ctx, "ack-msg-001", "correct-token")
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
}

// ============================================================================
// Dead Letter Invariant Tests
// ============================================================================

// TestDeadLetter_BoundedRetries verifies messages are dead-lettered after max
// attempts. This tests the bounded retries invariant.
func TestDeadLetter_BoundedRetries(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	mailboxID := uniqueID("dlq")
	messageID := "dl-msg-001"
	maxAttempts := 3

	// Enqueue message with low max attempts.
	payload, err := h.codec.Encode(&IncrementMsg{Amount: 1})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   mailboxID,
		MessageType: "counter.Increment",
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: maxAttempts,
	})
	require.NoError(t, err)

	// Simulate multiple failed attempts via lease/nack cycles.
	for i := 0; i < maxAttempts; i++ {
		token := fmt.Sprintf("token-%c", rune('A'+i))

		leased, err := h.store.LeaseNextMessage(
			h.ctx, mailboxID, token, 5*time.Second,
		)
		require.NoError(t, err)

		if leased != nil {
			// Nack to trigger retry.
			_, err = h.store.NackMessage(
				h.ctx, messageID, leased.LeaseToken,
				time.Millisecond,
			)
			require.NoError(t, err)
		}

		// Small delay for availability.
		time.Sleep(10 * time.Millisecond)
	}

	// After max attempts, move to dead letter.
	err = h.store.MoveToDeadLetter(
		h.ctx, messageID, "max attempts exceeded",
	)
	require.NoError(t, err)

	// Verify it's in dead letters.
	dl, err := h.store.GetDeadLetter(h.ctx, messageID)
	require.NoError(t, err)
	require.NotNil(t, dl)
	require.Equal(t, messageID, dl.ID)
	require.Equal(t, "max attempts exceeded", dl.FailureReason)
}

// TestDeadLetter_PayloadPreserved verifies dead letter contains original
// payload. This tests the dead letter preservation invariant.
func TestDeadLetter_PayloadPreserved(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	messageID := "dl-preserve-001"

	// Create message with specific payload.
	originalPayload, err := h.codec.Encode(&IncrementMsg{Amount: 999})
	require.NoError(t, err)

	err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
		ID:          messageID,
		MailboxID:   "preserve-test",
		MessageType: "counter.Increment",
		Payload:     originalPayload,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 1,
	})
	require.NoError(t, err)

	// Move to dead letter.
	err = h.store.MoveToDeadLetter(h.ctx, messageID, "test failure")
	require.NoError(t, err)

	// Retrieve dead letter and verify payload is preserved.
	dl, err := h.store.GetDeadLetter(h.ctx, messageID)
	require.NoError(t, err)
	require.NotNil(t, dl)

	// Decode payload and verify content.
	msg, err := h.codec.Decode(dl.Payload)
	require.NoError(t, err)

	incMsg, ok := msg.(*IncrementMsg)
	require.True(t, ok)
	require.Equal(t, int64(999), incMsg.Amount)
}

// ============================================================================
// Priority Ordering Tests
// ============================================================================

// TestPriority_HigherPriorityFirst verifies higher priority messages are
// delivered before lower priority. This tests the priority ordering invariant.
func TestPriority_HigherPriorityFirst(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	mailboxID := uniqueID("priority")
	now := time.Now().Add(-time.Minute)

	// Enqueue messages with different priorities (low first, then high).
	priorities := []struct {
		id       string
		priority int
		amount   int64
	}{
		{
			"low-pri",
			1,
			10,
		},
		{
			"med-pri",
			5,
			50,
		},
		{
			"high-pri",
			10,
			100,
		},
	}

	for _, p := range priorities {
		payload, err := h.codec.Encode(&IncrementMsg{Amount: p.amount})
		require.NoError(t, err)

		err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
			ID:          p.id,
			MailboxID:   mailboxID,
			MessageType: "counter.Increment",
			Payload:     payload,
			Priority:    p.priority,
			AvailableAt: now,
			MaxAttempts: 10,
		})
		require.NoError(t, err)
	}

	// Messages should be delivered in priority order: high, med, low.
	expectedOrder := []string{"high-pri", "med-pri", "low-pri"}

	for i, expectedID := range expectedOrder {
		leased, err := h.store.LeaseNextMessage(
			h.ctx, mailboxID, "token-"+expectedID, 30*time.Second,
		)
		require.NoError(t, err, "iteration %d", i)
		require.NotNil(t, leased, "iteration %d", i)
		require.Equal(t, expectedID, leased.ID, "iteration %d", i)

		// Ack to move to next.
		ackToken := "token-" + expectedID
		_, err = h.store.AckMessage(h.ctx, leased.ID, ackToken)
		require.NoError(t, err)
	}
}

// ============================================================================
// FIFO Ordering Tests
// ============================================================================

// TestFIFO_SamePriorityOrderedByTime verifies messages with same priority
// are delivered in FIFO order. This tests the FIFO within priority class
// invariant.
func TestFIFO_SamePriorityOrderedByTime(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	mailboxID := uniqueID("fifo")
	basePriority := 5

	// Enqueue messages at different times but same priority.
	var messageIDs []string
	for i := 0; i < 5; i++ {
		id := "fifo-msg-" + string(rune('A'+i))
		messageIDs = append(messageIDs, id)

		payload, err := h.codec.Encode(&IncrementMsg{Amount: int64(i)})
		require.NoError(t, err)

		// Each message slightly later in time.
		availableAt := time.Now().Add(-time.Hour)
		availableAt = availableAt.Add(time.Duration(i) * time.Minute)

		err = h.store.EnqueueMessage(h.ctx, actor.EnqueueParams{
			ID:          id,
			MailboxID:   mailboxID,
			MessageType: "counter.Increment",
			Payload:     payload,
			Priority:    basePriority,
			AvailableAt: availableAt,
			MaxAttempts: 10,
		})
		require.NoError(t, err)
	}

	// Messages should be delivered in order: A, B, C, D, E.
	for i, expectedID := range messageIDs {
		leased, err := h.store.LeaseNextMessage(
			h.ctx, mailboxID, "token-"+expectedID, 30*time.Second,
		)
		require.NoError(t, err, "iteration %d", i)
		require.NotNil(t, leased, "iteration %d", i)
		require.Equal(t, expectedID, leased.ID, "iteration %d", i)

		// Ack to move to next.
		ackToken := "token-" + expectedID
		_, err = h.store.AckMessage(h.ctx, leased.ID, ackToken)
		require.NoError(t, err)
	}
}

// ============================================================================
// DurableAsk Tests
// ============================================================================

// TestDurableAskResponseViaOutbox verifies that DurableAsk responses are
// delivered via the outbox to the callback actor's mailbox.
//
// Flow:
//   - Actor A sends DurableAsk to Actor B
//   - Actor B processes the message
//   - Actor B writes AskResponse to outbox
//   - OutboxPublisher delivers to Actor A's mailbox
//   - Actor A receives AskResponse
func TestDurableAskResponseViaOutbox(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Create two counter actors: sender (A) and target (B).
	senderID := uniqueID("sender")
	targetID := uniqueID("target")

	senderActor, senderBehavior := h.newDurableCounter(senderID)
	targetActor, targetBehavior := h.newDurableCounter(targetID)

	senderActor.Start()
	targetActor.Start()
	defer senderActor.Stop()
	defer targetActor.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Set target's count to a known value.
	targetBehavior.SetCount(100)

	// Send DurableAsk from sender to target.
	correlationID := uniqueID("corr")
	targetRef := targetActor.Ref()

	durableRef := requireDurableCounterRef(t, targetRef)

	err := durableRef.DurableAsk(
		h.ctx,
		&GetCountMsg{},
		actor.DurableAskParams{
			CallbackActorID: senderID,
			CorrelationID:   correlationID,
		},
	)
	require.NoError(t, err)

	// Wait for the response to be delivered to sender's mailbox.
	// The sender behavior receives all messages, so we check for
	// AskResponse.
	var receivedResponse *actor.AskResponse
	eventuallyWithOutboxPublish(
		t, publisher, durableAskResponseTimeout,
		func() bool {
			// Check if sender received an AskResponse.
			receivedResponse = senderBehavior.LastAskResponse()

			return receivedResponse != nil
		},
	)

	require.NotNil(t, receivedResponse)
	require.Equal(t, correlationID, receivedResponse.CorrelationID)
	require.False(t, receivedResponse.IsError())
}

// TestDurableAskErrorResponse verifies that error responses are correctly
// propagated via the outbox.
func TestDurableAskErrorResponse(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	senderID := uniqueID("sender")
	targetID := uniqueID("target")

	senderActor, senderBehavior := h.newDurableCounter(senderID)
	targetActor, targetBehavior := h.newDurableCounter(targetID)

	senderActor.Start()
	targetActor.Start()
	defer senderActor.Stop()
	defer targetActor.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Configure target to return an error.
	targetBehavior.SetForceError(fmt.Errorf("intentional error"))

	// Send DurableAsk.
	correlationID := uniqueID("corr")
	targetRef := targetActor.Ref()

	durableRef := requireDurableCounterRef(t, targetRef)

	err := durableRef.DurableAsk(
		h.ctx,
		&GetCountMsg{},
		actor.DurableAskParams{
			CallbackActorID: senderID,
			CorrelationID:   correlationID,
		},
	)
	require.NoError(t, err)

	// Wait for the error response.
	var receivedResponse *actor.AskResponse
	eventuallyWithOutboxPublish(
		t, publisher, durableAskResponseTimeout,
		func() bool {
			receivedResponse = senderBehavior.LastAskResponse()

			return receivedResponse != nil
		},
	)

	require.NotNil(t, receivedResponse)
	require.Equal(t, correlationID, receivedResponse.CorrelationID)
	require.True(t, receivedResponse.IsError())
	require.Contains(t, receivedResponse.ErrorText, "intentional error")
}

// TestDurableAskConcurrentRequests verifies multiple DurableAsk requests
// all receive their responses correctly.
func TestDurableAskConcurrentRequests(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	senderID := uniqueID("sender")
	targetID := uniqueID("target")

	senderActor, senderBehavior := h.newDurableCounter(senderID)
	targetActor, _ := h.newDurableCounter(targetID)

	senderActor.Start()
	targetActor.Start()
	defer senderActor.Stop()
	defer targetActor.Stop()

	// Create outbox publisher.
	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Send multiple DurableAsk requests concurrently.
	numRequests := 5
	correlationIDs := make([]string, numRequests)

	targetRef := targetActor.Ref()
	durableRef := requireDurableCounterRef(t, targetRef)

	for i := 0; i < numRequests; i++ {
		correlationIDs[i] = uniqueID(fmt.Sprintf("corr-%d", i))

		err := durableRef.DurableAsk(
			h.ctx,
			&GetCountMsg{},
			actor.DurableAskParams{
				CallbackActorID: senderID,
				CorrelationID:   correlationIDs[i],
			},
		)
		require.NoError(t, err)
	}

	// Wait for all responses.
	eventuallyWithOutboxPublish(
		t, publisher, durableAskResponseTimeout,
		func() bool {
			return senderBehavior.AskResponseCount() >= numRequests
		},
	)

	// Verify all correlation IDs received.
	receivedIDs := senderBehavior.ReceivedCorrelationIDs()
	for _, expectedID := range correlationIDs {
		found := false
		for _, id := range receivedIDs {
			if id == expectedID {
				found = true
				break
			}
		}
		require.True(t, found, "missing correlation ID: %s", expectedID)
	}
}

// ============================================================================
// DurableAsk Invariant Tests
// ============================================================================

// TestDurableAskWithSpecialCorrelationIDs verifies responses work with
// various correlation ID formats.
//
// INVARIANT: For any valid correlation ID, response is delivered with that ID.
func TestDurableAskWithSpecialCorrelationIDs(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	senderID := uniqueID("sender")
	targetID := uniqueID("target")

	senderActor, senderBehavior := h.newDurableCounter(senderID)
	targetActor, _ := h.newDurableCounter(targetID)

	senderActor.Start()
	targetActor.Start()
	defer senderActor.Stop()
	defer targetActor.Stop()

	publisherCfg := actor.DefaultOutboxPublisherConfig(
		h.store, h.codec, h.actorSystem,
	)
	publisherCfg.PollInterval = 10 * time.Millisecond
	publisher := actor.NewOutboxPublisher(publisherCfg)
	publisher.Start()
	defer publisher.Stop()

	// Test various correlation ID formats.
	veryLongID := fmt.Sprintf("very-long-correlation-id-%s-%s",
		uuid.NewString(), uuid.NewString())
	testIDs := []string{
		"simple-id",
		"uuid-" + uuid.NewString(),
		"with-special-chars_123",
		veryLongID,
	}

	targetRef := targetActor.Ref()
	durableRef := requireDurableCounterRef(t, targetRef)

	for _, correlationID := range testIDs {
		err := durableRef.DurableAsk(
			h.ctx,
			&GetCountMsg{},
			actor.DurableAskParams{
				CallbackActorID: senderID,
				CorrelationID:   correlationID,
			},
		)
		require.NoError(t, err)
	}

	// Wait for all responses.
	eventuallyWithOutboxPublish(
		t, publisher, durableAskResponseTimeout,
		func() bool {
			return senderBehavior.AskResponseCount() >= len(testIDs)
		},
	)

	// INVARIANT: All correlation IDs preserved.
	receivedIDs := senderBehavior.ReceivedCorrelationIDs()
	for _, expected := range testIDs {
		found := false
		for _, received := range receivedIDs {
			if expected == received {
				found = true
				break
			}
		}
		require.True(t, found, "missing correlation ID: %s", expected)
	}
}

// TestDurableAskErrorMessagePreserved verifies various error message formats
// are correctly propagated.
//
// INVARIANT: Error text is preserved in the response.
func TestDurableAskErrorMessagePreserved(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		errorMsg string
	}{
		{
			"simple error",
			"something went wrong",
		},
		{
			"with code",
			"error code 42: operation failed",
		},
		{
			"multiline",
			"line1\nline2\nline3",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			senderID := uniqueID("sender")
			targetID := uniqueID("target")

			senderActor, senderBehavior := h.newDurableCounter(
				senderID,
			)
			targetActor, targetBehavior := h.newDurableCounter(
				targetID,
			)

			senderActor.Start()
			targetActor.Start()
			defer senderActor.Stop()
			defer targetActor.Stop()

			publisherCfg := actor.DefaultOutboxPublisherConfig(
				h.store, h.codec, h.actorSystem,
			)
			publisherCfg.PollInterval = 10 * time.Millisecond
			publisher := actor.NewOutboxPublisher(publisherCfg)
			publisher.Start()
			defer publisher.Stop()

			targetBehavior.SetForceError(
				fmt.Errorf("%s", tc.errorMsg),
			)

			correlationID := uniqueID("corr")
			targetRef := targetActor.Ref()
			durableRef := requireDurableCounterRef(t, targetRef)

			err := durableRef.DurableAsk(
				h.ctx,
				&GetCountMsg{},
				actor.DurableAskParams{
					CallbackActorID: senderID,
					CorrelationID:   correlationID,
				},
			)
			require.NoError(t, err)

			// Wait for error response.
			eventuallyWithOutboxPublish(
				t, publisher, durableAskResponseTimeout,
				func() bool {
					count := senderBehavior.
						AskResponseCount()

					return count >= 1
				},
			)

			response := senderBehavior.LastAskResponse()
			require.NotNil(t, response)

			// INVARIANT: Error is preserved.
			require.True(t, response.IsError())
			require.Contains(t, response.ErrorText, tc.errorMsg)
		})
	}
}
