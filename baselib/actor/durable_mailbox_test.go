package actor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// durableTestMsg implements TLVMessage for testing DurableMailbox.
type durableTestMsg struct {
	BaseMessage
	Value   tlv.RecordT[tlv.TlvType1, uint64]
	Payload tlv.RecordT[tlv.TlvType2, []byte]
}

func (m *durableTestMsg) MessageType() string {
	return "durable.TestMsg"
}

func (m *durableTestMsg) TLVType() tlv.Type {
	return 0x2000
}

func (m *durableTestMsg) Encode(w io.Writer) error {
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

func (m *durableTestMsg) Decode(r io.Reader) error {
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

// durablePriorityTestMsg is a TLVMessage with priority.
type durablePriorityTestMsg struct {
	durableTestMsg
	priority int
}

func (m *durablePriorityTestMsg) TLVType() tlv.Type {
	return 0x2001 // Different from durableTestMsg.
}

func (m *durablePriorityTestMsg) MessageType() string {
	return "durable.PriorityTestMsg"
}

func (m *durablePriorityTestMsg) Priority() int {
	return m.priority
}

// newDurableTestCodec creates a MessageCodec for test messages.
func newDurableTestCodec() *MessageCodec {
	codec := NewMessageCodec()
	codec.MustRegister(0x2000, func() TLVMessage {
		return &durableTestMsg{}
	})
	codec.MustRegister(0x2001, func() TLVMessage {
		return &durablePriorityTestMsg{}
	})
	return codec
}

// TestDurableMailboxNewMailbox tests mailbox creation.
func TestDurableMailboxNewMailbox(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	require.NotNil(t, mailbox)
	require.False(t, mailbox.IsClosed())
	require.Equal(t, "test-mailbox", mailbox.cfg.MailboxID)
	require.Equal(t, 30*time.Second, mailbox.cfg.LeaseDuration)
	require.Equal(t, 100*time.Millisecond, mailbox.cfg.PollInterval)
	require.Equal(t, 10, mailbox.cfg.MaxAttempts)
}

// TestDurableMailboxSend tests that Send persists messages to the store.
func TestDurableMailboxSend(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Send should succeed and persist message.
	ok := mailbox.Send(ctx, env)
	require.True(t, ok)

	// Verify message was stored.
	store.mu.Lock()
	require.Len(t, store.messages, 1)
	for _, m := range store.messages {
		require.Equal(t, "test-mailbox", m.MailboxID)
		require.Equal(t, "durable.TestMsg", m.MessageType)
		require.NotEmpty(t, m.Payload)
	}
	store.mu.Unlock()
}

// TestDurableMailboxSendWithPriority tests that priority messages are handled.
func TestDurableMailboxSendWithPriority(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec() // Already has priority msg registered.

	ctx := context.Background()
	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durablePriorityTestMsg, int](ctx, cfg)

	msg := &durablePriorityTestMsg{
		durableTestMsg: durableTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(100)),
		},
		priority: 5,
	}

	env := envelope[*durablePriorityTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	ok := mailbox.Send(ctx, env)
	require.True(t, ok)

	// Verify priority was set.
	store.mu.Lock()
	require.Len(t, store.messages, 1)
	for _, m := range store.messages {
		require.Equal(t, 5, m.Priority)
	}
	store.mu.Unlock()
}

// TestDurableMailboxSendContextCancelled tests that Send respects context.
func TestDurableMailboxSendContextCancelled(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	// Create cancelled context.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: cancelledCtx,
	}

	// Send should fail with cancelled context.
	ok := mailbox.Send(cancelledCtx, env)
	require.False(t, ok)

	// Verify no message was stored.
	store.mu.Lock()
	require.Len(t, store.messages, 0)
	store.mu.Unlock()
}

// TestDurableMailboxSendActorContextCancelled tests that Send respects actor context.
func TestDurableMailboxSendActorContextCancelled(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()

	// Create actor context that's already cancelled.
	actorCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](actorCtx, cfg)

	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: context.Background(),
	}

	// Send should fail with cancelled actor context.
	ok := mailbox.Send(context.Background(), env)
	require.False(t, ok)
}

// TestDurableMailboxSendClosedMailbox tests that Send fails on closed mailbox.
func TestDurableMailboxSendClosedMailbox(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Close the mailbox.
	mailbox.Close()
	require.True(t, mailbox.IsClosed())

	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Send should fail on closed mailbox.
	ok := mailbox.Send(ctx, env)
	require.False(t, ok)
}

// TestDurableMailboxTrySend tests non-blocking send.
func TestDurableMailboxTrySend(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// TrySend should succeed.
	ok := mailbox.TrySend(env)
	require.True(t, ok)

	// Verify message was stored.
	store.mu.Lock()
	require.Len(t, store.messages, 1)
	store.mu.Unlock()
}

// TestDurableMailboxReceive tests receiving messages from the mailbox.
func TestDurableMailboxReceive(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Send a message first.
	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	ok := mailbox.Send(ctx, env)
	require.True(t, ok)

	// Receive should yield the message.
	var received *durableTestMsg
	receiveCtx, receiveCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer receiveCancel()

	for receivedEnv := range mailbox.Receive(receiveCtx) {
		received = receivedEnv.message
		break
	}

	require.NotNil(t, received)
	require.Equal(t, uint64(42), received.Value.Val)
	require.Equal(t, []byte("test"), received.Payload.Val)
}

// TestDurableMailboxReceiveContextCancelled tests that Receive respects context.
func TestDurableMailboxReceiveContextCancelled(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Create context that cancels immediately.
	receiveCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// Receive should return immediately.
	count := 0
	for range mailbox.Receive(receiveCtx) {
		count++
	}

	require.Equal(t, 0, count)
}

// TestDurableMailboxClose tests mailbox closure.
func TestDurableMailboxClose(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	require.False(t, mailbox.IsClosed())

	mailbox.Close()
	require.True(t, mailbox.IsClosed())

	// Double close should be safe.
	mailbox.Close()
	require.True(t, mailbox.IsClosed())
}

// TestDurableMailboxCloseStopsReceive tests that Close stops Receive iterator.
func TestDurableMailboxCloseStopsReceive(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = 10 * time.Millisecond
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	done := make(chan struct{})
	go func() {
		for range mailbox.Receive(ctx) {
			// Should not receive anything.
		}
		close(done)
	}()

	// Close the mailbox.
	time.Sleep(50 * time.Millisecond)
	mailbox.Close()

	// Receive should stop.
	select {
	case <-done:
		// Success.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Receive did not stop after Close")
	}
}

// TestDurableMailboxDrain tests that Drain returns empty for durable mailbox.
func TestDurableMailboxDrain(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Send a message.
	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	mailbox.Send(ctx, env)
	mailbox.Close()

	// Drain should return empty (messages stay in DB for recovery).
	count := 0
	for range mailbox.Drain() {
		count++
	}

	require.Equal(t, 0, count)
}

// TestDurableMailboxWakeSignal tests that wake channel triggers immediate poll.
func TestDurableMailboxWakeSignal(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = 1 * time.Hour // Long poll to ensure wake signal works.
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Start receiving in background.
	received := make(chan *durableTestMsg, 1)
	go func() {
		for env := range mailbox.Receive(ctx) {
			received <- env.message
			return
		}
	}()

	// Wait a bit then send a message.
	time.Sleep(50 * time.Millisecond)

	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Send triggers wake signal.
	mailbox.Send(ctx, env)

	// Should receive quickly despite long poll interval.
	select {
	case m := <-received:
		require.Equal(t, uint64(42), m.Value.Val)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive message after wake signal")
	}
}

// TestDurableMailboxConcurrentSends tests concurrent send operations.
func TestDurableMailboxConcurrentSends(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	const numSenders = 10
	const msgsPerSender = 100

	var wg sync.WaitGroup
	for i := 0; i < numSenders; i++ {
		wg.Add(1)
		go func(senderID int) {
			defer wg.Done()
			for j := 0; j < msgsPerSender; j++ {
				msg := &durableTestMsg{
					Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
						uint64(senderID*msgsPerSender + j),
					),
				}
				env := envelope[*durableTestMsg, int]{
					message:   msg,
					callerCtx: ctx,
				}
				mailbox.Send(ctx, env)
			}
		}(i)
	}

	wg.Wait()

	// All messages should be stored.
	store.mu.Lock()
	require.Len(t, store.messages, numSenders*msgsPerSender)
	store.mu.Unlock()
}

// TestDurableMailbox_DeliveryPassedInEnvelope verifies that the Delivery is
// passed directly in the envelope.delivery field, eliminating the need for
// global state.
func TestDurableMailbox_DeliveryPassedInEnvelope(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = 1 * time.Millisecond
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Send a message.
	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
	}
	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	ok := mailbox.Send(ctx, env)
	require.True(t, ok)

	// Receive the envelope and verify delivery is set.
	receiveCtx, receiveCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer receiveCancel()

	for receivedEnv := range mailbox.Receive(receiveCtx) {
		// The delivery should be passed directly in the envelope.
		require.NotNil(t, receivedEnv.delivery, "delivery should be set in envelope")

		// Type assertion should work.
		delivery, ok := receivedEnv.delivery.(*Delivery[*durableTestMsg, int])
		require.True(t, ok, "delivery should be correct type")
		require.NotEmpty(t, delivery.ID, "delivery should have ID")
		require.NotEmpty(t, delivery.LeaseToken, "delivery should have lease token")

		break
	}
}

// Property-based tests.

// TestDurableMailboxRapid_SendReceivePreservesData tests that data is preserved
// through send/receive cycle.
func TestDurableMailboxRapid_SendReceivePreservesData(t *testing.T) {
	t.Parallel()

	codec := newDurableTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
		cfg.PollInterval = 1 * time.Millisecond
		mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

		// Generate random values.
		value := rapid.Uint64().Draw(rt, "value")
		payload := rapid.SliceOf(rapid.Byte()).Draw(rt, "payload")

		msg := &durableTestMsg{
			Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](value),
			Payload: tlv.NewPrimitiveRecord[tlv.TlvType2](payload),
		}

		env := envelope[*durableTestMsg, int]{
			message:   msg,
			callerCtx: ctx,
		}

		ok := mailbox.Send(ctx, env)
		require.True(rt, ok)

		// Receive with timeout.
		receiveCtx, receiveCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer receiveCancel()

		var received *durableTestMsg
		for e := range mailbox.Receive(receiveCtx) {
			received = e.message
			break
		}

		require.NotNil(rt, received)
		require.Equal(rt, value, received.Value.Val)
		require.Equal(rt, payload, received.Payload.Val)
	})
}

// TestDurableMailboxRapid_ClosePreventsSend tests that close prevents all sends.
func TestDurableMailboxRapid_ClosePreventsSend(t *testing.T) {
	t.Parallel()

	codec := newDurableTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		ctx := context.Background()

		cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
		mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

		// Send some messages before close.
		numBefore := rapid.IntRange(0, 10).Draw(rt, "numBefore")
		for i := 0; i < numBefore; i++ {
			msg := &durableTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(i)),
			}
			env := envelope[*durableTestMsg, int]{
				message:   msg,
				callerCtx: ctx,
			}
			mailbox.Send(ctx, env)
		}

		// Close.
		mailbox.Close()

		// All subsequent sends should fail.
		numAfter := rapid.IntRange(1, 10).Draw(rt, "numAfter")
		for i := 0; i < numAfter; i++ {
			msg := &durableTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1000 + i)),
			}
			env := envelope[*durableTestMsg, int]{
				message:   msg,
				callerCtx: ctx,
			}
			ok := mailbox.Send(ctx, env)
			require.False(rt, ok, "send should fail after close")
		}

		// Only messages before close should be stored.
		store.mu.Lock()
		require.Len(rt, store.messages, numBefore)
		store.mu.Unlock()
	})
}

// TestDurableMailboxRapid_ConcurrentCloseAndSend tests safety of concurrent
// close and send operations.
func TestDurableMailboxRapid_ConcurrentCloseAndSend(t *testing.T) {
	t.Parallel()

	codec := newDurableTestCodec()

	rapid.Check(t, func(rt *rapid.T) {
		store := newMockDeliveryStore()
		ctx := context.Background()

		cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
		mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

		numSenders := rapid.IntRange(1, 5).Draw(rt, "numSenders")
		var wg sync.WaitGroup
		var closeCalled atomic.Bool

		// Start senders.
		for i := 0; i < numSenders; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					if closeCalled.Load() {
						return
					}
					msg := &durableTestMsg{
						Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
							uint64(id*10 + j),
						),
					}
					env := envelope[*durableTestMsg, int]{
						message:   msg,
						callerCtx: ctx,
					}
					mailbox.Send(ctx, env)
				}
			}(i)
		}

		// Close after random delay.
		time.Sleep(time.Duration(rapid.IntRange(0, 5).Draw(rt, "delay")) * time.Millisecond)
		closeCalled.Store(true)
		mailbox.Close()

		wg.Wait()

		// No panics or races should occur.
		require.True(rt, mailbox.IsClosed())
	})
}

// TestDurableMailboxPoisonMessageDeadLetter verifies that when a message
// consistently fails to decode and exhausts max_attempts, it is moved to the
// dead letter queue rather than being stranded in the mailbox.
// (Fix #5 from Codex review.)
func TestDurableMailboxPoisonMessageDeadLetter(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mailbox := NewDurableMailbox[*durableTestMsg, int](
		ctx,
		DurableMailboxConfig{
			MailboxID:     "test-actor",
			Store:         store,
			Codec:         codec,
			LeaseDuration: 30 * time.Second,
			PollInterval:  10 * time.Millisecond,
			MaxAttempts:   3,
		},
	)

	// Insert a message with corrupted payload that will fail to decode.
	// Set attempts to max so the first decode failure triggers dead-letter.
	poisonID := "poison-msg-1"
	store.mu.Lock()
	store.messages[poisonID] = &LeasedMessage{
		ID:          poisonID,
		MailboxID:   "test-actor",
		MessageType: "durable.TestMsg",
		Payload:     []byte("this is not valid TLV"),
		MaxAttempts: 3,
		Attempts:    3, // Already at max.
		CreatedAt:   time.Now(),
	}
	store.mu.Unlock()

	// Start receiving. The poison message should be dead-lettered.
	receiveCtx, receiveCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer receiveCancel()

	// Consume one iteration -- this will attempt to decode, fail, and
	// dead-letter since attempts >= max_attempts.
	for range mailbox.Receive(receiveCtx) {
		// Should not yield any valid envelope for the poison message.
		t.Fatal("should not receive a valid envelope for poison message")
	}

	// Verify the poison message was dead-lettered.
	store.mu.Lock()
	numDL := len(store.deadLetters)
	numMessages := len(store.messages)
	store.mu.Unlock()

	require.Equal(t, 1, numDL,
		"poison message should be in dead letter queue")
	require.Equal(t, 0, numMessages,
		"poison message should be removed from mailbox")
}

// TestDurableMailboxPoisonMessageNackBeforeMax verifies that a decode failure
// when attempts < max_attempts results in a nack (for retry) rather than
// dead-lettering. We use a very high MaxAttempts to ensure the message cannot
// exhaust during the brief test window.
func TestDurableMailboxPoisonMessageNackBeforeMax(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	// Use an extremely high max_attempts so even a tight decode-fail loop
	// cannot exhaust it during the test.
	const maxAttempts = 1_000_000

	mailbox := NewDurableMailbox[*durableTestMsg, int](
		ctx,
		DurableMailboxConfig{
			MailboxID:     "test-actor",
			Store:         store,
			Codec:         codec,
			LeaseDuration: 30 * time.Second,
			PollInterval:  10 * time.Millisecond,
			MaxAttempts:   maxAttempts,
		},
	)

	// Insert a poison message.
	poisonID := "poison-msg-2"
	store.mu.Lock()
	store.messages[poisonID] = &LeasedMessage{
		ID:          poisonID,
		MailboxID:   "test-actor",
		MessageType: "durable.TestMsg",
		Payload:     []byte("invalid TLV data"),
		MaxAttempts: maxAttempts,
		Attempts:    0,
		CreatedAt:   time.Now(),
	}
	store.mu.Unlock()

	// Receive very briefly (just enough for a few decode failures).
	receiveCtx, receiveCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer receiveCancel()

	for range mailbox.Receive(receiveCtx) {
		t.Fatal("should not receive a valid envelope for poison message")
	}

	// Message should still be in the mailbox (nacked, not dead-lettered).
	store.mu.Lock()
	numDL := len(store.deadLetters)
	numMessages := len(store.messages)
	attempts := 0
	if msg, ok := store.messages[poisonID]; ok {
		attempts = msg.Attempts
	}
	store.mu.Unlock()

	require.Equal(t, 0, numDL,
		"message should not be dead-lettered before max attempts")
	require.Equal(t, 1, numMessages,
		"message should remain in mailbox for retry")
	require.Greater(t, attempts, 0,
		"message should have been attempted at least once")
	require.Less(t, attempts, maxAttempts,
		"message should not have exhausted max attempts")
}

// TestDurableMailboxPromiseRegistryCleanupOnEnqueueFailure verifies that when
// EnqueueMessage fails, the promise registry entry is removed to prevent
// unbounded stale entries. (Fix #8 from Codex review.)
func TestDurableMailboxPromiseRegistryCleanupOnEnqueueFailure(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	mailbox := NewDurableMailbox[*durableTestMsg, int](
		ctx,
		DurableMailboxConfig{
			MailboxID:     "test-actor",
			Store:         store,
			Codec:         codec,
			LeaseDuration: 30 * time.Second,
			PollInterval:  100 * time.Millisecond,
			MaxAttempts:   10,
		},
	)

	// Inject enqueue error so Send will fail after promise registration.
	store.mu.Lock()
	store.injectEnqueueError = errors.New("simulated enqueue failure")
	store.mu.Unlock()

	// Attempt to Send an Ask envelope (with promise).
	promise := NewPromise[int]()
	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		promise:   promise,
		callerCtx: ctx,
	}

	// Send should return false due to enqueue failure.
	ok := mailbox.Send(ctx, env)
	require.False(t, ok)

	// The promise registry should be empty -- the entry should have been
	// cleaned up after the enqueue failure.
	mailbox.promiseRegistryMu.RLock()
	registrySize := len(mailbox.promiseRegistry)
	mailbox.promiseRegistryMu.RUnlock()

	require.Equal(t, 0, registrySize,
		"promise registry should be empty after enqueue failure")

	// Verify that repeated failures don't accumulate stale entries.
	for range 10 {
		p := NewPromise[int]()
		env := envelope[*durableTestMsg, int]{
			message:   msg,
			promise:   p,
			callerCtx: ctx,
		}
		ok := mailbox.Send(ctx, env)
		require.False(t, ok)
	}

	mailbox.promiseRegistryMu.RLock()
	registrySize = len(mailbox.promiseRegistry)
	mailbox.promiseRegistryMu.RUnlock()

	require.Equal(t, 0, registrySize,
		"promise registry should remain empty after repeated failures")
}

// TestDurableMailboxSendUsesOutboxIDFromContext verifies that when the context
// carries an outbox message ID (set by the OutboxPublisher), DurableMailbox.Send
// uses it as the inbox message ID instead of generating a fresh one. This
// enables receiver-side deduplication for CDC delivery retries.
// (Fix #2 from Codex review.)
func TestDurableMailboxSendUsesOutboxIDFromContext(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Inject the outbox ID into the context.
	outboxID := "outbox-msg-42"
	sendCtx := WithOutboxID(ctx, outboxID)

	ok := mailbox.Send(sendCtx, env)
	require.True(t, ok)

	// Verify the stored message uses the outbox ID, not a fresh UUID.
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(t, store.messages, 1)

	storedMsg, exists := store.messages[outboxID]
	require.True(t, exists,
		"message should be stored with outbox ID as key")
	require.Equal(t, outboxID, storedMsg.ID)
	require.Equal(t, "test-mailbox", storedMsg.MailboxID)
}

// TestDurableMailboxSendDuplicateOutboxIDIsIdempotent verifies that sending
// the same outbox-derived message ID twice is a no-op on the second attempt.
// This is the core receiver-side deduplication guarantee: if the OutboxPublisher
// retries after CompleteOutbox fails, the duplicate enqueue succeeds (returns
// true) without creating a second inbox message.
// (Fix #2 from Codex review.)
func TestDurableMailboxSendDuplicateOutboxIDIsIdempotent(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	outboxID := "outbox-msg-dedup"
	sendCtx := WithOutboxID(ctx, outboxID)

	// First send should succeed.
	ok := mailbox.Send(sendCtx, env)
	require.True(t, ok)

	// Second send with the same outbox ID should also succeed (idempotent).
	ok = mailbox.Send(sendCtx, env)
	require.True(t, ok)

	// Only one message should exist in the store.
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(t, store.messages, 1,
		"duplicate outbox ID should not create a second message")
	require.Contains(t, store.messages, outboxID)
}

// TestDurableMailboxSendWithoutOutboxIDGeneratesFreshID verifies that when
// no outbox ID is present in the context (normal Tell/Ask path), a fresh
// UUIDv7 is generated as before.
func TestDurableMailboxSendWithoutOutboxIDGeneratesFreshID(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Send without outbox ID in context (regular Tell path).
	ok := mailbox.Send(ctx, env)
	require.True(t, ok)

	// Verify a fresh UUIDv7 was generated (not empty, not a hardcoded value).
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(t, store.messages, 1)

	for id := range store.messages {
		require.NotEmpty(t, id)
		// UUIDv7 format: 8-4-4-4-12 hex chars with dashes.
		require.Len(t, id, 36,
			"generated ID should be a UUID (36 chars)")
	}
}

// txCapturingStore wraps mockDeliveryStore to capture the context passed to
// EnqueueMessage for inspection in tests.
type txCapturingStore struct {
	*mockDeliveryStore
	lastCtx context.Context
}

// EnqueueMessage captures the context before delegating to the underlying
// mock store.
func (s *txCapturingStore) EnqueueMessage(ctx context.Context,
	params EnqueueParams) error {

	s.lastCtx = ctx

	return s.mockDeliveryStore.EnqueueMessage(ctx, params)
}

// TestDurableMailboxSendStripsSenderTx verifies that Send strips the sender's
// database transaction from the context before calling EnqueueMessage. This
// prevents the receiver's delivery store from inheriting the sender's
// transaction, which would cause cross-DB visibility issues or Ask deadlocks.
func TestDurableMailboxSendStripsSenderTx(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	// Wrap the store to capture the context passed to EnqueueMessage.
	capturing := &txCapturingStore{mockDeliveryStore: store}

	cfg := DefaultDurableMailboxConfig("test-mailbox", capturing, codec)
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("test")),
	}

	env := envelope[*durableTestMsg, int]{
		message:   msg,
		callerCtx: ctx,
	}

	// Create a context with a transaction attached, simulating a
	// sender inside an ExecTx closure.
	sendCtx := WithTx(ctx, (*sql.Tx)(nil))
	require.True(t, HasTx(sendCtx))

	// Send should succeed.
	ok := mailbox.Send(sendCtx, env)
	require.True(t, ok)

	// The context received by EnqueueMessage should NOT carry a tx.
	require.NotNil(t, capturing.lastCtx,
		"EnqueueMessage should have been called")
	require.False(t, HasTx(capturing.lastCtx),
		"EnqueueMessage context should not carry the sender's tx")
}
