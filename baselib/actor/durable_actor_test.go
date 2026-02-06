package actor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// actorTestMsg implements TLVMessage for DurableActor testing.
type actorTestMsg struct {
	BaseMessage
	Value   tlv.RecordT[tlv.TlvType1, uint64]
	Payload tlv.RecordT[tlv.TlvType2, []byte]
}

func (m *actorTestMsg) MessageType() string {
	return "actor.TestMsg"
}

func (m *actorTestMsg) TLVType() tlv.Type {
	return 0x3000
}

func (m *actorTestMsg) Encode(w io.Writer) error {
	records := []tlv.Record{
		m.Value.Record(),
		m.Payload.Record(),
	}
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}
	return stream.Encode(w)
}

func (m *actorTestMsg) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(
		m.Value.Record(),
		m.Payload.Record(),
	)
	if err != nil {
		return err
	}
	_, err = stream.DecodeWithParsedTypes(r)
	return err
}

// newActorTestCodec creates a MessageCodec for actor test messages.
func newActorTestCodec() *MessageCodec {
	codec := NewMessageCodec()
	codec.MustRegister(0x3000, func() TLVMessage {
		return &actorTestMsg{}
	})
	return codec
}

// mockBehavior is a test implementation of ActorBehavior.
type mockBehavior struct {
	mu sync.Mutex

	// receiveCalls tracks all received messages.
	receiveCalls []*actorTestMsg

	// result is the result to return from Receive.
	result fn.Result[int]

	// delay is how long to wait before returning.
	delay time.Duration

	// panicOnReceive causes Receive to panic.
	panicOnReceive bool

	// onReceive is called when a message is received (before returning).
	onReceive func(ctx context.Context, msg *actorTestMsg)
}

func newMockBehavior(result fn.Result[int]) *mockBehavior {
	return &mockBehavior{
		result: result,
	}
}

func (b *mockBehavior) Receive(ctx context.Context, msg *actorTestMsg) fn.Result[int] {
	b.mu.Lock()
	b.receiveCalls = append(b.receiveCalls, msg)
	result := b.result
	delay := b.delay
	panicOnReceive := b.panicOnReceive
	onReceive := b.onReceive
	b.mu.Unlock()

	if onReceive != nil {
		onReceive(ctx, msg)
	}

	if panicOnReceive {
		panic("behavior panic")
	}

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fn.Err[int](ctx.Err())
		}
	}

	return result
}

func (b *mockBehavior) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.receiveCalls)
}

func (b *mockBehavior) setResult(result fn.Result[int]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.result = result
}

func (b *mockBehavior) setDelay(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delay = d
}

// stoppableMockBehavior extends mockBehavior with Stoppable interface.
type stoppableMockBehavior struct {
	*mockBehavior
	stopCalled atomic.Bool
	stopErr    error
}

func newStoppableMockBehavior(result fn.Result[int]) *stoppableMockBehavior {
	return &stoppableMockBehavior{
		mockBehavior: newMockBehavior(result),
	}
}

func (b *stoppableMockBehavior) OnStop(ctx context.Context) error {
	b.stopCalled.Store(true)
	return b.stopErr
}

// mockTxAwareStore extends mockDeliveryStore with TxAwareDeliveryStore.
type mockTxAwareStore struct {
	*mockDeliveryStore

	// txExecuted tracks whether ExecTx was called.
	txExecuted atomic.Bool

	// txCount tracks how many times ExecTx was called.
	txCount atomic.Int32

	// txShouldFail causes ExecTx to fail.
	txShouldFail bool

	// nackCalled tracks whether NackMessage was called after tx failure.
	nackCalled atomic.Bool
}

func newMockTxAwareStore() *mockTxAwareStore {
	return &mockTxAwareStore{
		mockDeliveryStore: newMockDeliveryStore(),
	}
}

func (m *mockTxAwareStore) ExecTx(
	ctx context.Context,
	readOnly bool,
	fn TxFunc,
) error {

	m.txExecuted.Store(true)
	m.txCount.Add(1)

	if m.txShouldFail {
		return errors.New("simulated tx failure")
	}

	// Execute the function with the same store (simulating a transaction).
	return fn(ctx, m.mockDeliveryStore)
}

// Override NackMessage to track calls.
func (m *mockTxAwareStore) NackMessage(
	ctx context.Context,
	id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	m.nackCalled.Store(true)

	return m.mockDeliveryStore.NackMessage(ctx, id, leaseToken, retryAfter)
}

// TestDurableActorCreation tests actor creation with various configs.
func TestDurableActorCreation(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	require.NotNil(t, actor)
	require.Equal(t, "test-actor", actor.id)
	require.Equal(t, 30*time.Second, actor.leaseDuration)
	require.Equal(t, 10*time.Second, actor.heartbeatInterval)
	require.Equal(t, 5*time.Second, actor.cleanupTimeout)
	require.Equal(t, 24*time.Hour, actor.deduplicationTTL)
}

// TestDurableActorStartStop tests basic lifecycle.
func TestDurableActorStartStop(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	// Start should be idempotent.
	actor.Start()
	actor.Start()

	// Give the goroutine time to start.
	time.Sleep(10 * time.Millisecond)

	// Stop should be idempotent.
	actor.Stop()
	actor.Stop()

	// Give the goroutine time to stop.
	time.Sleep(50 * time.Millisecond)
}

// TestDurableActorTellProcessing tests Tell message processing.
func TestDurableActorTellProcessing(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	// Send a message.
	msg := &actorTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("hello")),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)
	require.NoError(t, err)

	// Wait for processing.
	require.Eventually(t, func() bool {
		return behavior.callCount() == 1
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Verify message was acked (removed from store).
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.messages) == 0
	}, 500*time.Millisecond, 10*time.Millisecond)
}

// TestDurableActorAskProcessing tests Ask message processing.
func TestDurableActorAskProcessing(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	// Send an Ask message.
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(99)),
	}

	ctx := context.Background()
	future := actor.Ref().Ask(ctx, msg)

	// Wait for result.
	resultCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	result := future.Await(resultCtx)

	val, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, 42, val)
}

// TestDurableActorAskWithError tests Ask returns error from behavior.
func TestDurableActorAskWithError(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	expectedErr := errors.New("behavior error")
	behavior := newMockBehavior(fn.Err[int](expectedErr))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(99)),
	}

	ctx := context.Background()
	future := actor.Ref().Ask(ctx, msg)

	resultCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	result := future.Await(resultCtx)

	// Ask always acks even with error - the error is part of the result.
	require.Error(t, result.Err())
	require.Equal(t, expectedErr.Error(), result.Err().Error())
}

// TestDurableActorDeduplication tests that duplicate messages are skipped.
func TestDurableActorDeduplication(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	// Send first message.
	msg1 := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg1)
	require.NoError(t, err)

	// Wait for processing.
	require.Eventually(t, func() bool {
		return behavior.callCount() == 1
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Get the message ID that was processed.
	store.mu.Lock()
	var processedIDs []string
	for id := range store.processed {
		processedIDs = append(processedIDs, id)
	}
	store.mu.Unlock()

	require.Len(t, processedIDs, 1)

	// Re-enqueue the same message ID (simulating redelivery).
	store.mu.Lock()
	payload, _ := codec.Encode(msg1)
	store.messages[processedIDs[0]] = &LeasedMessage{
		ID:          processedIDs[0],
		MailboxID:   "test-actor",
		MessageType: msg1.MessageType(),
		Payload:     payload,
		MaxAttempts: 10,
		Attempts:    1,
		CreatedAt:   time.Now(),
	}
	store.mu.Unlock()

	// Wake the mailbox to process it.
	actor.mailbox.wake <- struct{}{}

	// Wait and verify no additional processing occurred.
	time.Sleep(100 * time.Millisecond)

	// Should still only have 1 call (duplicate was skipped).
	require.Equal(t, 1, behavior.callCount())
}

// TestDurableActorPanicRecovery tests that panics are recovered.
func TestDurableActorPanicRecovery(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))
	behavior.panicOnReceive = true

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond

	// Custom retry policy that gives up immediately.
	cfg.TellRetryPolicy = func(err error, attempts int) (bool, time.Duration) {
		return false, 0
	}

	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)
	require.NoError(t, err)

	// Wait for processing (should not crash).
	time.Sleep(100 * time.Millisecond)

	// Actor should still be running.
	require.NoError(t, actor.ctx.Err())
}

// TestDurableActorTellRetryPolicy tests that Tell respects retry policy.
func TestDurableActorTellRetryPolicy(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()

	callCount := atomic.Int32{}
	behavior := newMockBehavior(fn.Err[int](errors.New("fail")))
	behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
		callCount.Add(1)
	}

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond

	// Retry policy that allows 3 attempts with short delay.
	cfg.TellRetryPolicy = func(err error, attempts int) (bool, time.Duration) {
		if attempts >= 3 {
			return false, 0
		}
		return true, 10 * time.Millisecond
	}

	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)
	require.NoError(t, err)

	// Wait for retries to complete.
	require.Eventually(t, func() bool {
		return callCount.Load() >= 3
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Message should be in dead letters after max retries.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.deadLetters) >= 1 || len(store.messages) == 0
	}, 500*time.Millisecond, 10*time.Millisecond)
}

// TestDurableActorTransactionWrapping tests that processing uses transactions
// when store supports it.
func TestDurableActorTransactionWrapping(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)
	require.NoError(t, err)

	// Wait for processing.
	require.Eventually(t, func() bool {
		return behavior.callCount() == 1
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Transaction should have been used.
	require.True(t, store.txExecuted.Load())
}

// TestDurableActorTransactionFailure tests that tx failure causes nack.
func TestDurableActorTransactionFailure(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	store.txShouldFail = true
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)
	require.NoError(t, err)

	// Verify message was enqueued.
	store.mu.Lock()
	initialCount := len(store.messages)
	store.mu.Unlock()
	t.Logf("After Tell: %d messages in store", initialCount)
	require.Equal(t, 1, initialCount, "message should be enqueued")

	// Wait for first transaction to be attempted.
	require.Eventually(t, func() bool {
		return store.txExecuted.Load()
	}, 500*time.Millisecond, 10*time.Millisecond)
	t.Log("Transaction was executed")

	// Wait for nack to be called.
	require.Eventually(t, func() bool {
		return store.nackCalled.Load()
	}, 500*time.Millisecond, 10*time.Millisecond)
	t.Logf("Nack was called. TX count: %d", store.txCount.Load())

	// Immediately check message count (before actor can process again).
	store.mu.Lock()
	numMessages := len(store.messages)
	numDL := len(store.deadLetters)
	t.Logf("After first tx failure: %d messages in store, %d in dead letters", numMessages, numDL)
	store.mu.Unlock()

	// With txShouldFail=true permanently, the message keeps retrying until
	// max attempts is reached and it gets dead-lettered.
	// For this test, we want to verify the message wasn't lost.
	// Either it's still in messages (waiting for retry) or in dead letters.
	require.True(t, numMessages >= 1 || numDL >= 1,
		"message should either be in store for retry or in dead letters")
}

// TestDurableActorStoppableBehavior tests that Stoppable.OnStop is called.
func TestDurableActorStoppableBehavior(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newStoppableMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.CleanupTimeout = 1 * time.Second
	actor := NewDurableActor(cfg)

	actor.Start()
	time.Sleep(10 * time.Millisecond)
	actor.Stop()

	// Wait for cleanup.
	time.Sleep(100 * time.Millisecond)

	require.True(t, behavior.stopCalled.Load())
}

// TestDurableActorRef tests the ActorRef implementation.
func TestDurableActorRef(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	ref := actor.Ref()
	require.NotNil(t, ref)
	require.Equal(t, "test-actor", ref.ID())

	// TellRef should return same underlying ref.
	tellRef := actor.TellRef()
	require.NotNil(t, tellRef)
}

// TestDurableActorTellToTerminatedActor tests Tell to stopped actor.
func TestDurableActorTellToTerminatedActor(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	actor.Start()
	actor.Stop()

	// Wait for actor to fully stop.
	time.Sleep(100 * time.Millisecond)

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	err := actor.Ref().Tell(ctx, msg)

	require.Error(t, err)
	require.Equal(t, ErrActorTerminated, err)
}

// TestDurableActorAskToTerminatedActor tests Ask to stopped actor.
func TestDurableActorAskToTerminatedActor(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	actor.Start()
	actor.Stop()

	// Wait for actor to fully stop.
	time.Sleep(100 * time.Millisecond)

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	ctx := context.Background()
	future := actor.Ref().Ask(ctx, msg)

	result := future.Await(ctx)
	require.Error(t, result.Err())
	require.Equal(t, ErrActorTerminated, result.Err())
}

// TestDurableActorWithWaitGroup tests lifecycle tracking with WaitGroup.
func TestDurableActorWithWaitGroup(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	var wg sync.WaitGroup
	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.Wg = &wg

	actor := NewDurableActor(cfg)

	actor.Start()
	actor.Stop()

	// WaitGroup should complete when actor stops.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitGroup did not complete after actor stop")
	}
}

// TestDefaultTellRetryPolicy tests the default retry policy.
func TestDefaultTellRetryPolicy(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		attempts      int
		expectRetry   bool
		expectMaxSecs int
	}{
		{attempts: 0, expectRetry: true, expectMaxSecs: 2},
		{attempts: 1, expectRetry: true, expectMaxSecs: 4},
		{attempts: 2, expectRetry: true, expectMaxSecs: 8},
		{attempts: 3, expectRetry: true, expectMaxSecs: 16},
		{attempts: 4, expectRetry: true, expectMaxSecs: 60},
		{attempts: 5, expectRetry: false, expectMaxSecs: 0},
		{attempts: 100, expectRetry: false, expectMaxSecs: 0},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("attempts_%d", tc.attempts), func(t *testing.T) {
			retry, delay := DefaultTellRetryPolicy(
				errors.New("test"), tc.attempts,
			)
			require.Equal(t, tc.expectRetry, retry)
			if tc.expectRetry {
				require.LessOrEqual(t, delay.Seconds(), float64(tc.expectMaxSecs))
			}
		})
	}
}

// Property-based tests.

// TestDurableActorRapid_DeduplicationIdempotent verifies deduplication.
func TestDurableActorRapid_DeduplicationIdempotent(t *testing.T) {
	t.Parallel()

	codec := newActorTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		callCount := atomic.Int32{}

		behavior := newMockBehavior(fn.Ok(42))
		behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
			callCount.Add(1)
		}

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 1 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		// Generate random message.
		value := rapid.Uint64().Draw(rt, "value")
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
		}

		ctx := context.Background()
		err := actor.Ref().Tell(ctx, msg)
		require.NoError(rt, err)

		// Wait for first processing.
		require.Eventually(rt, func() bool {
			return callCount.Load() == 1
		}, 500*time.Millisecond, 5*time.Millisecond)

		// Get processed ID.
		store.mu.Lock()
		var processedID string
		for id := range store.processed {
			processedID = id
			break
		}
		payload, _ := codec.Encode(msg)
		// Re-enqueue same ID.
		store.messages[processedID] = &LeasedMessage{
			ID:          processedID,
			MailboxID:   "test-actor",
			MessageType: msg.MessageType(),
			Payload:     payload,
			MaxAttempts: 10,
			Attempts:    1,
			CreatedAt:   time.Now(),
		}
		store.mu.Unlock()

		// Trigger re-processing.
		select {
		case actor.mailbox.wake <- struct{}{}:
		default:
		}

		// Wait and verify still only 1 call.
		time.Sleep(50 * time.Millisecond)
		require.Equal(rt, int32(1), callCount.Load(),
			"duplicate message should be skipped")
	})
}

// TestDurableActorRapid_AckAfterSuccess verifies ack on success.
func TestDurableActorRapid_AckAfterSuccess(t *testing.T) {
	t.Parallel()

	codec := newActorTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		behavior := newMockBehavior(fn.Ok(42))

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 1 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		// Generate random message.
		value := rapid.Uint64().Draw(rt, "value")
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
		}

		ctx := context.Background()
		err := actor.Ref().Tell(ctx, msg)
		require.NoError(rt, err)

		// Wait for processing.
		require.Eventually(rt, func() bool {
			return behavior.callCount() == 1
		}, 100*time.Millisecond, 1*time.Millisecond)

		// Message should be removed (acked).
		require.Eventually(rt, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			return len(store.messages) == 0
		}, 100*time.Millisecond, 1*time.Millisecond)
	})
}

// TestDurableActorRapid_NackAfterFailure verifies nack on failure.
func TestDurableActorRapid_NackAfterFailure(t *testing.T) {
	t.Parallel()

	codec := newActorTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		callCount := atomic.Int32{}

		behavior := newMockBehavior(fn.Err[int](errors.New("fail")))
		behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
			callCount.Add(1)
		}

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 1 * time.Millisecond

		// Allow 2 retries with short delay.
		cfg.TellRetryPolicy = func(err error, attempts int) (bool, time.Duration) {
			if attempts >= 2 {
				return false, 0
			}
			return true, 1 * time.Millisecond
		}

		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		// Generate random message.
		value := rapid.Uint64().Draw(rt, "value")
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
		}

		ctx := context.Background()
		err := actor.Ref().Tell(ctx, msg)
		require.NoError(rt, err)

		// Wait for retries.
		require.Eventually(rt, func() bool {
			return callCount.Load() >= 2
		}, 500*time.Millisecond, 10*time.Millisecond)

		// After max retries, message should be removed.
		require.Eventually(rt, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			// Either dead-lettered or removed.
			return len(store.messages) == 0 || len(store.deadLetters) > 0
		}, 500*time.Millisecond, 10*time.Millisecond)
	})
}

// TestDurableActorRapid_ConcurrentTellSafe verifies concurrent Tell safety.
func TestDurableActorRapid_ConcurrentTellSafe(t *testing.T) {
	t.Parallel()

	codec := newActorTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		callCount := atomic.Int32{}

		behavior := newMockBehavior(fn.Ok(42))
		behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
			callCount.Add(1)
		}

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 1 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		numMessages := rapid.IntRange(5, 20).Draw(rt, "numMessages")
		numSenders := rapid.IntRange(2, 5).Draw(rt, "numSenders")

		var wg sync.WaitGroup
		for s := 0; s < numSenders; s++ {
			wg.Add(1)
			go func(senderID int) {
				defer wg.Done()
				for i := 0; i < numMessages; i++ {
					msg := &actorTestMsg{
						Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
							uint64(senderID*1000 + i),
						),
					}
					ctx := context.Background()
					actor.Ref().Tell(ctx, msg)
				}
			}(s)
		}

		wg.Wait()

		// Wait for all messages to be processed.
		expectedCalls := int32(numSenders * numMessages)
		require.Eventually(rt, func() bool {
			return callCount.Load() == expectedCalls
		}, 1*time.Second, 5*time.Millisecond,
			"expected %d calls, got %d", expectedCalls, callCount.Load())
	})
}

// TestDurableAskValidation tests error validation in DurableAsk.
func TestDurableAskValidation(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	ctx := context.Background()
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	durableRef := actor.Ref().(DurableActorRef[*actorTestMsg, int])

	t.Run("empty callback actor ID", func(t *testing.T) {
		err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "",
			CorrelationID:   "test-correlation",
		})

		require.Error(t, err)
		require.Contains(t, err.Error(), "callback actor ID is required")
	})

	t.Run("empty correlation ID", func(t *testing.T) {
		err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "callback-actor",
			CorrelationID:   "",
		})

		require.Error(t, err)
		require.Contains(t, err.Error(), "correlation ID is required")
	})

	t.Run("both empty", func(t *testing.T) {
		err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "",
			CorrelationID:   "",
		})

		require.Error(t, err)
		// First check is callback actor ID.
		require.Contains(t, err.Error(), "callback actor ID is required")
	})

	t.Run("valid params", func(t *testing.T) {
		err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "callback-actor",
			CorrelationID:   "test-correlation",
		})

		// Should succeed (message enqueued).
		require.NoError(t, err)
	})
}

// TestDurableAskToStoppedActor tests DurableAsk behavior when actor is stopped.
func TestDurableAskToStoppedActor(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	actor.Start()
	actor.Stop()

	// Wait for actor to fully stop.
	time.Sleep(100 * time.Millisecond)

	ctx := context.Background()
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	durableRef := actor.Ref().(DurableActorRef[*actorTestMsg, int])

	err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
		CallbackActorID: "callback-actor",
		CorrelationID:   "test-correlation",
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrActorTerminated)
}

// TestDurableActorWithTxAwareStore tests message processing with transactions.
func TestDurableActorWithTxAwareStore(t *testing.T) {
	t.Parallel()

	t.Run("uses transactions for message processing", func(t *testing.T) {
		t.Parallel()

		store := newMockTxAwareStore()
		codec := newActorTestCodec()
		behavior := newMockBehavior(fn.Ok(42))

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 10 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		ctx := context.Background()
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		}

		err := actor.Ref().Tell(ctx, msg)
		require.NoError(t, err)

		// Wait for processing.
		require.Eventually(t, func() bool {
			return behavior.callCount() >= 1
		}, 500*time.Millisecond, 5*time.Millisecond)

		// Transaction should have been executed.
		require.True(t, store.txExecuted.Load())
		require.GreaterOrEqual(t, store.txCount.Load(), int32(1))
	})

	t.Run("transaction failure triggers nack", func(t *testing.T) {
		t.Parallel()

		store := newMockTxAwareStore()
		store.txShouldFail = true
		codec := newActorTestCodec()
		behavior := newMockBehavior(fn.Ok(42))

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 10 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		ctx := context.Background()
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		}

		err := actor.Ref().Tell(ctx, msg)
		require.NoError(t, err)

		// Wait for transaction attempt.
		require.Eventually(t, func() bool {
			return store.txExecuted.Load()
		}, 500*time.Millisecond, 5*time.Millisecond)

		// Nack should be called on tx failure.
		require.Eventually(t, func() bool {
			return store.nackCalled.Load()
		}, 500*time.Millisecond, 5*time.Millisecond)
	})

	t.Run("durable ask with transaction", func(t *testing.T) {
		t.Parallel()

		store := newMockTxAwareStore()
		codec := newActorTestCodec()
		// Register AskResponse in the codec.
		codec.MustRegister(AskResponseMsgType, func() TLVMessage {
			return &AskResponse{}
		})
		behavior := newMockBehavior(fn.Ok(42))

		cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
		cfg.PollInterval = 10 * time.Millisecond
		actor := NewDurableActor(cfg)

		actor.Start()
		defer actor.Stop()

		ctx := context.Background()
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		}

		durableRef := actor.Ref().(DurableActorRef[*actorTestMsg, int])

		err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "callback-actor",
			CorrelationID:   "test-correlation",
		})
		require.NoError(t, err)

		// Wait for processing.
		require.Eventually(t, func() bool {
			return behavior.callCount() >= 1
		}, 500*time.Millisecond, 5*time.Millisecond)

		// Transaction should have been used.
		require.True(t, store.txExecuted.Load())

		// Outbox should contain the response.
		require.Eventually(t, func() bool {
			store.mockDeliveryStore.mu.Lock()
			count := len(store.mockDeliveryStore.outbox)
			store.mockDeliveryStore.mu.Unlock()
			return count >= 1
		}, 500*time.Millisecond, 5*time.Millisecond)

		// Verify the outbox message.
		store.mockDeliveryStore.mu.Lock()
		require.NotEmpty(t, store.mockDeliveryStore.outbox)
		var outboxMsg *OutboxMessage
		for _, msg := range store.mockDeliveryStore.outbox {
			outboxMsg = msg
			break
		}
		require.Equal(t, "callback-actor", outboxMsg.TargetActorID)
		store.mockDeliveryStore.mu.Unlock()
	})
}

// TestDurableAskNacksOnOutboxWriteFailure verifies that when
// writeAskResponseToOutbox fails for a DurableAsk, the message is nacked for
// retry instead of being acked and permanently dropping the response.
// (Fix #1 from Codex review.)
func TestDurableAskNacksOnOutboxWriteFailure(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	codec.MustRegister(AskResponseMsgType, func() TLVMessage {
		return &AskResponse{}
	})

	// Use a channel-signaled behavior so we can stop the actor after the
	// first processing and inspect state before the retry loop churns
	// through all attempts.
	firstCall := make(chan struct{})
	behavior := newMockBehavior(fn.Ok(42))
	behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
		select {
		case firstCall <- struct{}{}:
		default:
		}
	}

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()

	ctx := context.Background()
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	// Inject outbox error to simulate write failure.
	store.mu.Lock()
	store.injectOutboxError = errors.New("simulated outbox failure")
	store.mu.Unlock()

	durableRef := actor.Ref().(DurableActorRef[*actorTestMsg, int])

	err := durableRef.DurableAsk(ctx, msg, DurableAskParams{
		CallbackActorID: "callback-actor",
		CorrelationID:   "test-correlation",
	})
	require.NoError(t, err)

	// Wait for first call, then stop the actor to prevent retry churn.
	select {
	case <-firstCall:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("behavior was never called")
	}

	// Brief pause for nack to complete, then stop actor.
	time.Sleep(20 * time.Millisecond)
	actor.Stop()
	time.Sleep(20 * time.Millisecond)

	store.mu.Lock()
	numProcessed := len(store.processed)
	numOutbox := len(store.outbox)
	store.mu.Unlock()

	require.Equal(t, 0, numProcessed,
		"message should not be marked processed when outbox write fails")
	require.Equal(t, 0, numOutbox,
		"outbox should be empty when write fails")

	// The behavior was called, confirming the message was processed but
	// the outbox write failure caused a nack (not an ack).
	require.GreaterOrEqual(t, behavior.callCount(), 1)
}

// TestPromiseCompletionDeferredUntilAfterAck verifies that in the non-tx path,
// the Ask promise is completed only after AckMessage succeeds, not before.
// (Fix #3 from Codex review.)
func TestPromiseCompletionDeferredUntilAfterAck(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	promise := NewPromise[string]()
	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		Promise:     promise,
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Ack should succeed.
	err := delivery.Ack(ctx, fn.Ok("the result"))
	require.NoError(t, err)

	// Promise should be completed after Ack returns.
	result := promise.Future().Await(ctx)
	value, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, "the result", value)

	// Message should be removed from store.
	require.Empty(t, store.messages)
}

// TestPromiseNotCompletedOnAckFailure verifies that if AckMessage fails
// (e.g., lease expired), the promise is not completed.
func TestPromiseNotCompletedOnAckFailure(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	// Store has a DIFFERENT lease token, so Ack will return 0 rows.
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  "different-token",
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	promise := NewPromise[string]()
	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		Promise:     promise,
		LeaseToken:  "stale-token",
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Ack should fail with ErrLeaseExpired.
	err := delivery.Ack(ctx, fn.Ok("the result"))
	require.ErrorIs(t, err, ErrLeaseExpired)

	// Promise should NOT be completed (no result available yet).
	promiseCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	result := promise.Future().Await(promiseCtx)

	// The await should time out because the promise was never completed.
	require.Error(t, result.Err())
}

// TestPromiseCompletionDeferredInTxPath verifies that in the tx path, the
// promise is only completed after ExecTx returns (i.e., after commit).
func TestPromiseCompletionDeferredInTxPath(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(99)),
	}

	ctx := context.Background()
	future := actor.Ref().Ask(ctx, msg)

	// Wait for result with timeout.
	resultCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	result := future.Await(resultCtx)

	val, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, 42, val)

	// Transaction should have been used.
	require.True(t, store.txExecuted.Load())
}

// TestPromiseNotCompletedOnTxFailure verifies that if the transaction fails,
// the in-memory promise is NOT completed.
func TestPromiseNotCompletedOnTxFailure(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	store.txShouldFail = true
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(42))

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(99)),
	}

	ctx := context.Background()
	future := actor.Ref().Ask(ctx, msg)

	// Wait for tx failure + nack.
	require.Eventually(t, func() bool {
		return store.txExecuted.Load()
	}, 500*time.Millisecond, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return store.nackCalled.Load()
	}, 500*time.Millisecond, 10*time.Millisecond)

	// The promise should NOT have been completed.
	promiseCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	result := future.Await(promiseCtx)

	// Should time out or get context error - not a real result.
	require.Error(t, result.Err())
}

// TestDeliveryConcurrentExtendAndAck verifies that concurrent Extend and Ack
// calls on a Delivery do not race. This test should be run with -race.
// (Fix #4 from Codex review.)
func TestDeliveryConcurrentExtendAndAck(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	// Run concurrent Extend calls alongside an Ack.
	var wg sync.WaitGroup
	done := make(chan struct{})

	// Heartbeat-like goroutine that calls Extend.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				// Extend may return ErrAlreadyAcked after Ack
				// completes, which is expected.
				_ = delivery.Extend(ctx, 30*time.Second)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Also read LeaseRemaining and IsLeaseExpired concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = delivery.LeaseRemaining()
				_ = delivery.IsLeaseExpired()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Let the concurrent access run for a bit.
	time.Sleep(20 * time.Millisecond)

	// Ack on the main goroutine.
	err := delivery.Ack(ctx, fn.Ok("success"))
	require.NoError(t, err)

	// Signal goroutines to stop.
	close(done)
	wg.Wait()
}

// TestDeliveryConcurrentExtendAndNack is the same as above but with Nack.
func TestDeliveryConcurrentExtendAndNack(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	msgID := "test-msg-1"
	leaseToken := "test-lease-token"
	store.messages[msgID] = &LeasedMessage{
		ID:          msgID,
		MailboxID:   "test-actor",
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
	}

	delivery := &Delivery[*testTLVMsg, string]{
		ID:          msgID,
		Message:     &testTLVMsg{Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42))},
		LeaseToken:  leaseToken,
		LeaseUntil:  time.Now().Add(30 * time.Second),
		Attempts:    1,
		MaxAttempts: 10,
		store:       store,
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Heartbeat-like goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = delivery.Extend(ctx, 30*time.Second)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)

	// Nack on the main goroutine.
	err := delivery.Nack(ctx, errors.New("error"), 5*time.Second)
	require.NoError(t, err)

	close(done)
	wg.Wait()
}

// TestDurableAskWithMailboxFull tests DurableAsk behavior when mailbox is full.
func TestDurableAskWithMailboxFull(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()

	// Create a behavior that blocks on receive.
	behavior := newMockBehavior(fn.Ok(42))
	behavior.setDelay(5 * time.Second)

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	actor := NewDurableActor(cfg)

	actor.Start()
	defer actor.Stop()

	ctx := context.Background()

	// Fill the mailbox by sending messages that will block.
	// The mailbox has a default size, so we need to fill it.
	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	durableRef := actor.Ref().(DurableActorRef[*actorTestMsg, int])

	// Send many messages to fill the mailbox.
	for i := 0; i < 200; i++ {
		_ = durableRef.DurableAsk(ctx, msg, DurableAskParams{
			CallbackActorID: "callback-actor",
			CorrelationID:   fmt.Sprintf("test-correlation-%d", i),
		})
	}

	// Use a context with timeout that will be exceeded.
	ctxTimeout, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
	defer cancel()

	// Wait for context to expire.
	time.Sleep(5 * time.Millisecond)

	// This should fail with ErrMailboxFull or context.DeadlineExceeded.
	err := durableRef.DurableAsk(ctxTimeout, msg, DurableAskParams{
		CallbackActorID: "callback-actor",
		CorrelationID:   "test-correlation-overflow",
	})

	// Either mailbox is full or context deadline exceeded - both are acceptable.
	require.Error(t, err)
}
