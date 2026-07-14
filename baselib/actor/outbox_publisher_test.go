package actor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// outboxTestMsg implements TLVMessage for OutboxPublisher testing.
type outboxTestMsg struct {
	BaseMessage
	Value tlv.RecordT[tlv.TlvType1, uint64]
}

func (m *outboxTestMsg) MessageType() string {
	return "outbox.TestMsg"
}

func (m *outboxTestMsg) TLVType() tlv.Type {
	return 0x4000
}

func (m *outboxTestMsg) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(m.Value.Record())
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

func (m *outboxTestMsg) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(m.Value.Record())
	if err != nil {
		return err
	}
	_, err = stream.DecodeWithParsedTypes(r)

	return err
}

// newOutboxTestCodec creates a MessageCodec for outbox test messages.
func newOutboxTestCodec() *MessageCodec {
	codec := NewMessageCodec()
	codec.MustRegister(0x4000, func() TLVMessage {
		return &outboxTestMsg{}
	})

	return codec
}

// mockSystem implements SystemContext for testing.
type mockSystem struct {
	mu sync.Mutex

	// receptionist is the actor registry.
	receptionist *Receptionist

	// tellCalls tracks Tell calls for verification.
	tellCalls []struct {
		target string
		msg    Message
		ctx    context.Context
	}

	// tellError is returned from Tell if non-nil.
	tellError error
}

func newMockSystem() *mockSystem {
	s := &mockSystem{
		receptionist: newReceptionist(),
	}

	// Pre-register mock actors for the targets we'll use.
	s.registerMockActor("target-actor")
	s.registerMockActor("target")

	return s
}

func (s *mockSystem) registerMockActor(name string) {
	mockRef := &mockActorRef{system: s, target: name}
	key := NewServiceKey[Message, any](name)
	_ = RegisterWithReceptionist(s.receptionist, key, mockRef)
}

// Receptionist returns the receptionist.
func (s *mockSystem) Receptionist() *Receptionist {
	return s.receptionist
}

// DeadLetters returns a reference to the dead letter actor.
func (s *mockSystem) DeadLetters() ActorRef[Message, any] {
	return nil // Not used in these tests.
}

// mockActorRef implements ActorRef for testing.
type mockActorRef struct {
	system *mockSystem
	target string
}

func (r *mockActorRef) ID() string {
	return r.target
}

func (r *mockActorRef) Tell(ctx context.Context, msg Message) error {
	r.system.mu.Lock()
	defer r.system.mu.Unlock()

	r.system.tellCalls = append(r.system.tellCalls, struct {
		target string
		msg    Message
		ctx    context.Context
	}{
		target: r.target,
		msg:    msg,
		ctx:    ctx,
	})

	return r.system.tellError
}

func (r *mockActorRef) Ask(ctx context.Context, msg Message) Future[any] {
	promise := NewPromise[any]()
	promise.Complete(fn.Err[any](errors.New("Ask not supported in mock")))

	return promise.Future()
}

// TestOutboxPublisherCreation tests publisher creation.
func TestOutboxPublisherCreation(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	publisher := NewOutboxPublisher(cfg)

	require.NotNil(t, publisher)
	require.Equal(t, time.Second, publisher.cfg.PollInterval)
	require.Equal(t, 100, publisher.cfg.BatchSize)
	require.Equal(t, 10, publisher.cfg.MaxDeliveryAttempts)
}

// TestOutboxPublisherStartStop tests lifecycle.
func TestOutboxPublisherStartStop(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	publisher := NewOutboxPublisher(cfg)

	// Start should be idempotent.
	publisher.Start()
	publisher.Start()

	// Give time for goroutine to start.
	time.Sleep(20 * time.Millisecond)

	// Stop should be idempotent.
	publisher.Stop()
	publisher.Stop()
}

// TestOutboxPublisherDelivery tests message delivery.
func TestOutboxPublisherDelivery(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	// Create an outbox message.
	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	outboxMsg := &OutboxMessage{
		ID:            "outbox-1",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
		Status:        "pending",
	}

	store.mu.Lock()
	store.outbox[outboxMsg.ID] = outboxMsg
	store.mu.Unlock()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	publisher := NewOutboxPublisher(cfg)

	publisher.Start()
	defer publisher.Stop()

	// Wait for message to be delivered.
	require.Eventually(t, func() bool {
		system.mu.Lock()
		defer system.mu.Unlock()

		return len(system.tellCalls) > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Verify Tell was called with correct target.
	system.mu.Lock()
	require.Len(t, system.tellCalls, 1)
	require.Equal(t, "target-actor", system.tellCalls[0].target)
	system.mu.Unlock()

	// Verify message was marked complete.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		msg, ok := store.outbox["outbox-1"]

		return ok && msg.Status == "completed"
	}, 500*time.Millisecond, 10*time.Millisecond)
}

var errPostDeliveryTxFailure = errors.New("post-delivery tx failure")

type postDeliveryFailTxStore struct {
	*mockDeliveryStore

	execCalled atomic.Bool
}

func (s *postDeliveryFailTxStore) ExecTx(ctx context.Context, readOnly bool,
	fn TxFunc) error {

	s.execCalled.Store(true)

	if err := fn(ctx, s.mockDeliveryStore); err != nil {
		return err
	}

	return errPostDeliveryTxFailure
}

// TestOutboxPublisherLogsExecTxFailure verifies commit-level transaction
// failures are visible even when the inner delivery closure completed
// successfully and therefore emitted no delivery-specific warning.
func TestOutboxPublisherLogsExecTxFailure(t *testing.T) {
	t.Parallel()

	baseStore := newMockDeliveryStore()
	store := &postDeliveryFailTxStore{
		mockDeliveryStore: baseStore,
	}
	codec := newOutboxTestCodec()
	system := newMockSystem()

	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	baseStore.mu.Lock()
	baseStore.outbox["outbox-tx-fail"] = &OutboxMessage{
		ID:            "outbox-tx-fail",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
		Status:        "pending",
	}
	baseStore.mu.Unlock()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = time.Hour
	publisher := NewOutboxPublisher(cfg)

	var logBuf bytes.Buffer
	handler := btclog.NewDefaultHandler(&logBuf)
	log := btclog.NewSLogger(handler.SubSystem("TEST"))
	log.SetLevel(btclog.LevelWarn)
	publisher.ctx = build.ContextWithLogger(publisher.ctx, log)

	publisher.PublishPending()

	require.True(t, store.execCalled.Load())
	require.Contains(
		t, logBuf.String(),
		"Failed to execute outbox delivery transaction",
	)
	require.Contains(t, logBuf.String(), errPostDeliveryTxFailure.Error())
	require.Contains(t, logBuf.String(), "outbox-tx-fail")

	system.mu.Lock()
	require.Len(t, system.tellCalls, 1)
	system.mu.Unlock()

	baseStore.mu.Lock()
	status := baseStore.outbox["outbox-tx-fail"].Status
	baseStore.mu.Unlock()
	require.Equal(t, "completed", status)
}

// TestOutboxPublisherDecodeError tests poison pill handling.
func TestOutboxPublisherDecodeError(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	// Create an outbox message with invalid payload.
	outboxMsg := &OutboxMessage{
		ID:            "outbox-1",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "unknown.Type",
		Payload:       []byte("invalid payload"),
		Status:        "pending",
	}

	store.mu.Lock()
	store.outbox[outboxMsg.ID] = outboxMsg
	store.mu.Unlock()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	publisher := NewOutboxPublisher(cfg)

	publisher.Start()
	defer publisher.Stop()

	// Wait for message to be failed (dead-lettered).
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		msg, ok := store.outbox["outbox-1"]

		return ok && msg.Status == "dead_letter"
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Verify Tell was NOT called.
	system.mu.Lock()
	require.Len(t, system.tellCalls, 0)
	system.mu.Unlock()
}

// TestOutboxPublisherDeliveryError tests handling of Tell errors.
func TestOutboxPublisherDeliveryError(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()
	system.tellError = errors.New("delivery failed")

	// Create an outbox message.
	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	outboxMsg := &OutboxMessage{
		ID:            "outbox-1",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
		Status:        "pending",
	}

	store.mu.Lock()
	store.outbox[outboxMsg.ID] = outboxMsg
	store.mu.Unlock()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	publisher := NewOutboxPublisher(cfg)

	publisher.Start()
	defer publisher.Stop()

	// Wait for Tell to be attempted.
	require.Eventually(t, func() bool {
		system.mu.Lock()
		defer system.mu.Unlock()

		return len(system.tellCalls) > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Message should still be pending (for retry).
	store.mu.Lock()
	msg2, ok := store.outbox["outbox-1"]
	require.True(t, ok)
	require.Equal(t, "pending", msg2.Status)
	store.mu.Unlock()
}

// TestOutboxPublisherBatching tests batch processing.
func TestOutboxPublisherBatching(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	// Create multiple outbox messages.
	for i := 0; i < 5; i++ {
		msg := &outboxTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(i)),
		}
		payload, err := codec.Encode(msg)
		require.NoError(t, err)

		outboxMsg := &OutboxMessage{
			ID:            generateID(),
			SourceActorID: "source-actor",
			TargetActorID: "target-actor",
			MessageType:   msg.MessageType(),
			Payload:       payload,
			Status:        "pending",
		}

		store.mu.Lock()
		store.outbox[outboxMsg.ID] = outboxMsg
		store.mu.Unlock()
	}

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	cfg.BatchSize = 10 // Should get all 5 in one batch.
	publisher := NewOutboxPublisher(cfg)

	publisher.Start()
	defer publisher.Stop()

	// Wait for all messages to be completed.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, m := range store.outbox {
			if m.Status != "completed" {
				return false
			}
		}

		return true
	}, 500*time.Millisecond, 10*time.Millisecond)

	// All 5 messages should have been delivered.
	system.mu.Lock()
	require.Len(t, system.tellCalls, 5)
	system.mu.Unlock()
}

// TestOutboxPublisherPublishPending tests manual publish trigger.
func TestOutboxPublisherPublishPending(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	// Create an outbox message.
	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	outboxMsg := &OutboxMessage{
		ID:            "outbox-1",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
		Status:        "pending",
	}

	store.mu.Lock()
	store.outbox[outboxMsg.ID] = outboxMsg
	store.mu.Unlock()

	// Use long poll interval.
	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 1 * time.Hour
	publisher := NewOutboxPublisher(cfg)

	// Don't start the publisher - use manual trigger.
	publisher.PublishPending()

	// Message should be delivered immediately.
	system.mu.Lock()
	require.Len(t, system.tellCalls, 1)
	system.mu.Unlock()
}

// TestOutboxPublisherWakesOnEnqueue verifies same-process outbox enqueue wakes
// the publisher without waiting for its polling fallback.
func TestOutboxPublisherWakesOnEnqueue(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = time.Hour
	publisher := NewOutboxPublisher(cfg)
	publisher.Start()
	defer publisher.Stop()

	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	err = store.EnqueueOutbox(t.Context(), OutboxParams{
		ID:            "outbox-wake",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		system.mu.Lock()
		defer system.mu.Unlock()

		return len(system.tellCalls) == 1
	}, 500*time.Millisecond, 10*time.Millisecond)
}

// TestOutboxPublisherPropagatesOutboxID verifies that the OutboxPublisher
// injects the outbox message ID into the context when calling Tell on the
// target actor. This is the publisher-side half of the receiver-side
// deduplication mechanism: the outbox row ID flows through context → Tell →
// DurableMailbox.Send → EnqueueMessage, so retry deliveries produce the same
// inbox message ID and the ON CONFLICT clause deduplicates them.
// (Fix #2 from Codex review.)
func TestOutboxPublisherPropagatesOutboxID(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newOutboxTestCodec()
	system := newMockSystem()

	// Create an outbox message with a known ID.
	msg := &outboxTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(99)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	outboxID := "outbox-dedup-42"
	outboxMsg := &OutboxMessage{
		ID:            outboxID,
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   msg.MessageType(),
		Payload:       payload,
		Status:        "pending",
	}

	store.mu.Lock()
	store.outbox[outboxMsg.ID] = outboxMsg
	store.mu.Unlock()

	cfg := DefaultOutboxPublisherConfig(store, codec, system)
	cfg.PollInterval = 10 * time.Millisecond
	publisher := NewOutboxPublisher(cfg)

	publisher.Start()
	defer publisher.Stop()

	// Wait for the message to be delivered.
	require.Eventually(t, func() bool {
		system.mu.Lock()
		defer system.mu.Unlock()

		return len(system.tellCalls) > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Verify the context passed to Tell carries the outbox message ID.
	system.mu.Lock()
	require.Len(t, system.tellCalls, 1)

	call := system.tellCalls[0]
	propagatedID, ok := OutboxIDFromContext(call.ctx)
	require.True(t, ok,
		"Tell context should carry outbox ID")
	require.Equal(
		t, outboxID, propagatedID,
		"propagated outbox ID should match original",
	)
	system.mu.Unlock()
}

// Property-based tests.

// TestOutboxPublisherRapid_EventualDelivery verifies eventual delivery.
func TestOutboxPublisherRapid_EventualDelivery(t *testing.T) {
	t.Parallel()

	codec := newOutboxTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		system := newMockSystem()

		// Generate random number of messages.
		numMessages := rapid.IntRange(1, 10).Draw(rt, "numMessages")

		for i := 0; i < numMessages; i++ {
			value := rapid.Uint64().Draw(rt, "value")
			msg := &outboxTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
					value,
				),
			}
			payload, _ := codec.Encode(msg)

			outboxMsg := &OutboxMessage{
				ID:            generateID(),
				SourceActorID: "source",
				TargetActorID: "target",
				MessageType:   msg.MessageType(),
				Payload:       payload,
				Status:        "pending",
			}

			store.mu.Lock()
			store.outbox[outboxMsg.ID] = outboxMsg
			store.mu.Unlock()
		}

		cfg := DefaultOutboxPublisherConfig(store, codec, system)
		cfg.PollInterval = 1 * time.Millisecond
		publisher := NewOutboxPublisher(cfg)

		publisher.Start()
		defer publisher.Stop()

		// All messages should eventually be completed.
		require.Eventually(rt, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			for _, m := range store.outbox {
				if m.Status != "completed" {
					return false
				}
			}

			return true
		}, 1*time.Second, 10*time.Millisecond)

		// Verify correct number of Tell calls.
		system.mu.Lock()
		require.Equal(rt, numMessages, len(system.tellCalls))
		system.mu.Unlock()
	})
}

// TestOutboxPublisherRapid_NoDoubleDDelivery verifies no duplicate delivery.
func TestOutboxPublisherRapid_NoDoubleDelivery(t *testing.T) {
	t.Parallel()

	codec := newOutboxTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		system := newMockSystem()

		// Create a single message.
		msg := &outboxTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		}
		payload, _ := codec.Encode(msg)

		messageID := generateID()
		outboxMsg := &OutboxMessage{
			ID:            messageID,
			SourceActorID: "source",
			TargetActorID: "target",
			MessageType:   msg.MessageType(),
			Payload:       payload,
			Status:        "pending",
		}

		store.mu.Lock()
		store.outbox[outboxMsg.ID] = outboxMsg
		store.mu.Unlock()

		cfg := DefaultOutboxPublisherConfig(store, codec, system)
		cfg.PollInterval = 1 * time.Millisecond
		publisher := NewOutboxPublisher(cfg)

		publisher.Start()
		defer publisher.Stop()

		// Wait for delivery (status changes to completed).
		require.Eventually(rt, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			m, ok := store.outbox[messageID]

			return ok && m.Status == "completed"
		}, 1*time.Second, 10*time.Millisecond)

		// Wait a bit more to ensure no extra deliveries.
		time.Sleep(50 * time.Millisecond)

		// Should be exactly one Tell call.
		system.mu.Lock()
		require.Equal(
			rt, 1, len(system.tellCalls),
			"message should be delivered exactly once",
		)
		system.mu.Unlock()
	})
}

// TestOutboxPublisherRapid_ConcurrentPublish tests concurrent behavior.
func TestOutboxPublisherRapid_ConcurrentPublish(t *testing.T) {
	t.Parallel()

	codec := newOutboxTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		system := newMockSystem()

		cfg := DefaultOutboxPublisherConfig(store, codec, system)
		cfg.PollInterval = 1 * time.Millisecond
		publisher := NewOutboxPublisher(cfg)

		publisher.Start()
		defer publisher.Stop()

		// Concurrently add messages while publisher is running.
		numWriters := rapid.IntRange(2, 5).Draw(rt, "numWriters")
		msgsPerWriter := rapid.IntRange(3, 10).Draw(rt, "msgsPerWriter")
		totalMessages := numWriters * msgsPerWriter

		var wg sync.WaitGroup
		messageCount := atomic.Int32{}

		for w := 0; w < numWriters; w++ {
			wg.Add(1)
			go func(writerID int) {
				defer wg.Done()
				for i := 0; i < msgsPerWriter; i++ {
					msg := &outboxTestMsg{
						Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
							uint64(
								writerID*
									1000 +
									i,
							),
						),
					}
					payload, _ := codec.Encode(msg)

					outboxMsg := &OutboxMessage{
						ID:            generateID(),
						SourceActorID: "source",
						TargetActorID: "target",
						MessageType:   msg.MessageType(),
						Payload:       payload,
						Status:        "pending",
					}

					store.mu.Lock()
					store.outbox[outboxMsg.ID] = outboxMsg
					store.mu.Unlock()

					messageCount.Add(1)
				}
			}(w)
		}

		wg.Wait()

		// All messages should eventually be completed.
		require.Eventually(rt, func() bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			for _, m := range store.outbox {
				if m.Status != "completed" {
					return false
				}
			}

			return true
		}, 2*time.Second, 20*time.Millisecond)

		// Verify all messages were delivered.
		system.mu.Lock()
		require.Equal(rt, totalMessages, len(system.tellCalls))
		system.mu.Unlock()
	})
}
