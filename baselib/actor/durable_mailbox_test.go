package actor

import (
	"context"
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
