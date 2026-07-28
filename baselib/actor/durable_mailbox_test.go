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
	require.Equal(t, time.Second, mailbox.cfg.PollInterval)
	require.Equal(t, 30*time.Second, mailbox.cfg.MaxPollInterval)
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
	err := mailbox.Send(ctx, env)
	require.NoError(t, err)

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

	err := mailbox.Send(ctx, env)
	require.NoError(t, err)

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
	err := mailbox.Send(cancelledCtx, env)
	require.ErrorIs(t, err, context.Canceled)

	// Verify no message was stored.
	store.mu.Lock()
	require.Len(t, store.messages, 0)
	store.mu.Unlock()
}

// TestDurableMailboxSendActorContextCancelled tests that Send respects actor
// context.
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
	err := mailbox.Send(context.Background(), env)
	require.ErrorIs(t, err, ErrActorTerminated)
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
	err := mailbox.Send(ctx, env)
	require.ErrorIs(t, err, ErrMailboxClosed)
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
	err := mailbox.TrySend(env)
	require.NoError(t, err)

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

	err := mailbox.Send(ctx, env)
	require.NoError(t, err)

	// Receive should yield the message.
	var received *durableTestMsg
	receiveCtx, receiveCancel := context.WithTimeout(
		ctx, 500*time.Millisecond,
	)
	defer receiveCancel()

	for receivedEnv := range mailbox.Receive(receiveCtx) {
		received = receivedEnv.message
		break
	}

	require.NotNil(t, received)
	require.Equal(t, uint64(42), received.Value.Val)
	require.Equal(t, []byte("test"), received.Payload.Val)
}

// TestDurableMailboxReceiveLeaselessNackBackoff verifies that when a leaseless
// delivery's nack store write fails, the receive loop backs off for a poll
// interval before re-peeking instead of tight-spinning. The failed nack leaves
// the row physically unchanged and immediately re-eligible, and without the
// backoff the loop would re-peek the same row in an unbounded CPU-bound tight
// loop. We assert that the number of peeks over a fixed window stays bounded by
// the poll cadence rather than growing without bound.
func TestDurableMailboxReceiveLeaselessNackBackoff(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const pollInterval = 25 * time.Millisecond

	cfg := DefaultDurableMailboxConfig("test-mailbox", store, codec)
	cfg.PollInterval = pollInterval
	cfg.SingleWorkerLeaseless = true
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// Seed one eligible message directly so the peek path returns it.
	msg := &durableTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(7)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("x")),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	store.messages["leaseless-msg"] = &LeasedMessage{
		ID:          "leaseless-msg",
		MailboxID:   "test-mailbox",
		MessageType: msg.MessageType(),
		Payload:     payload,
		Attempts:    0,
		MaxAttempts: 10,
	}

	// Make every leaseless nack store write fail while leaving the message
	// peek-eligible, so each delivery is flagged mutationFailed and the
	// loop must throttle before re-peeking the same unchanged row.
	store.injectNackError = errors.New("database is locked")

	receiveCtx, receiveCancel := context.WithTimeout(
		ctx, 5*pollInterval,
	)
	defer receiveCancel()

	deliveries := 0
	for receivedEnv := range mailbox.Receive(receiveCtx) {
		deliveries++

		delivery, ok :=
			receivedEnv.delivery.(*Delivery[*durableTestMsg, int])
		require.True(t, ok)

		// Nack fails against the wedged store, setting mutationFailed.
		nackErr := delivery.Nack(
			receiveCtx, errors.New("transient"), pollInterval,
		)
		require.Error(t, nackErr)
		require.True(t, delivery.MutationFailed())
	}

	// Over a ~5 poll-interval window, a throttled loop peeks on the order
	// of the poll count, not thousands of times. A generous ceiling still
	// distinguishes bounded backoff from an unbounded tight spin.
	peeks := store.peekCount.Load()
	require.Less(
		t, peeks, int64(50),
		"receive loop tight-spun instead of backing off: %d peeks",
		peeks,
	)
	require.Positive(t, deliveries)
}

// TestDurableMailboxReceiveContextCancelled tests that Receive respects
// context.
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

	require.NoError(t, mailbox.Send(ctx, env))
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
	cfg.PollInterval = 1 *
		time.Hour // Long poll to ensure wake signal works.
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
	require.NoError(t, mailbox.Send(ctx, env))

	// Should receive quickly despite long poll interval.
	select {
	case m := <-received:
		require.Equal(t, uint64(42), m.Value.Val)

	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive message after wake signal")
	}
}

// TestDurableMailboxRegistersMailboxWake verifies that NewDurableMailbox
// registers its Wake with a store that implements MailboxWakeRegistrar, and
// that firing the registered wake rouses the receive loop. This stands in for
// the folded outbox-delivery path: the publisher's ExecTx enqueues into this
// mailbox inside its write tx (so Send's pre-commit wake races ahead of the
// row becoming visible), then fires the registered wake after commit. Without
// it the consumer would idle until its poll interval.
func TestDurableMailboxRegistersMailboxWake(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A long poll interval ensures only the registered wake can deliver in
	// time, isolating the post-commit wake path from the poll fallback.
	cfg := DefaultDurableMailboxConfig("wake-mailbox", store, codec)
	cfg.PollInterval = time.Hour
	mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

	// The mailbox must have registered exactly one post-commit wake.
	store.mu.Lock()
	registered := len(store.mailboxWakes)
	store.mu.Unlock()
	require.Equal(
		t, 1, registered, "mailbox did not register a wake callback",
	)

	received := make(chan *durableTestMsg, 1)
	go func() {
		for env := range mailbox.Receive(ctx) {
			received <- env.message

			return
		}
	}()

	// Let the receive loop settle into its long poll wait.
	time.Sleep(50 * time.Millisecond)

	// Enqueue the row directly (bypassing Send's pre-commit wake) to model
	// a row that only becomes visible at commit, then fire the registered
	// post-commit wake the way the store does after ExecTx commits.
	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(7)),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	require.NoError(
		t,
		store.EnqueueMessage(
			ctx, EnqueueParams{
				ID:          "wake-msg",
				MailboxID:   "wake-mailbox",
				MessageType: msg.MessageType(),
				Payload:     payload,
				AvailableAt: time.Now().Add(-time.Minute),
				MaxAttempts: 3,
			},
		),
	)

	store.fireMailboxWakes()

	select {
	case m := <-received:
		require.Equal(t, uint64(7), m.Value.Val)

	case <-time.After(500 * time.Millisecond):
		t.Fatal(
			"registered mailbox wake did not rouse the receive " +
				"loop",
		)
	}
}

// TestDurableMailboxWakeRestartNoAccumulation verifies that constructing a
// fresh DurableMailbox for the same durable ID against a shared store (the
// actor-restart pattern) does not accumulate stale wake closures: each
// construction registers one wake and Close cancels it, so at any time the
// store holds only the live mailboxes' wakes. Without the cancel-on-Close
// contract, every restart would leave another inert closure that
// notifyMailboxWake walks on each folded enqueue commit, an unbounded leak on
// the feature's primary execution path.
func TestDurableMailboxWakeRestartNoAccumulation(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const mailboxID = "restart-mailbox"

	// Simulate many actor restarts that each reuse the same durable ID and
	// the same store. Each constructed mailbox registers, then Close
	// cancels it, leaving the store with no live wake between restarts.
	const restarts = 50
	for i := 0; i < restarts; i++ {
		cfg := DefaultDurableMailboxConfig(mailboxID, store, codec)
		mailbox := NewDurableMailbox[*durableTestMsg, int](ctx, cfg)

		// While live, exactly one wake is registered.
		store.mu.Lock()
		liveCount := len(store.mailboxWakes)
		store.mu.Unlock()
		require.Equal(
			t, 1, liveCount,
			"wake map grew beyond the single live mailbox",
		)

		mailbox.Close()

		// After Close, the entry is gone entirely.
		store.mu.Lock()
		afterClose := len(store.mailboxWakes)
		store.mu.Unlock()
		require.Equal(
			t, 0, afterClose,
			"Close left a stale wake closure behind",
		)
	}

	// The map must not have grown with the restart count.
	store.mu.Lock()
	final := len(store.mailboxWakes)
	store.mu.Unlock()
	require.Equal(t, 0, final,
		"wake closures accumulated across restarts")
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
						uint64(
							senderID*
								msgsPerSender +
								j,
						),
					),
				}
				env := envelope[*durableTestMsg, int]{
					message:   msg,
					callerCtx: ctx,
				}
				_ = mailbox.Send(ctx, env)
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

	err := mailbox.Send(ctx, env)
	require.NoError(t, err)

	// Receive the envelope and verify delivery is set.
	receiveCtx, receiveCancel := context.WithTimeout(
		ctx, 100*time.Millisecond,
	)
	defer receiveCancel()

	for receivedEnv := range mailbox.Receive(receiveCtx) {
		// The delivery should be passed directly in the envelope.
		require.NotNil(
			t, receivedEnv.delivery,
			"delivery should be set in envelope",
		)

		// Type assertion should work.
		delivery, ok :=
			receivedEnv.delivery.(*Delivery[*durableTestMsg, int])
		require.True(t, ok, "delivery should be correct type")
		require.NotEmpty(t, delivery.ID, "delivery should have ID")
		require.NotEmpty(
			t, delivery.LeaseToken,
			"delivery should have lease token",
		)

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

		err := mailbox.Send(ctx, env)
		require.NoError(rt, err)

		// Receive with timeout.
		receiveCtx, receiveCancel := context.WithTimeout(
			ctx, 100*time.Millisecond,
		)
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

// TestDurableMailboxRapid_ClosePreventsSend tests that close prevents all
// sends.
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
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
					uint64(i),
				),
			}
			env := envelope[*durableTestMsg, int]{
				message:   msg,
				callerCtx: ctx,
			}
			require.NoError(rt, mailbox.Send(ctx, env))
		}

		// Close.
		mailbox.Close()

		// All subsequent sends should fail.
		numAfter := rapid.IntRange(1, 10).Draw(rt, "numAfter")
		for i := 0; i < numAfter; i++ {
			msg := &durableTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
					uint64(1000 + i),
				),
			}
			env := envelope[*durableTestMsg, int]{
				message:   msg,
				callerCtx: ctx,
			}
			err := mailbox.Send(ctx, env)
			require.ErrorIs(
				rt, err, ErrMailboxClosed,
				"send should fail after close",
			)
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
					_ = mailbox.Send(ctx, env)
				}
			}(i)
		}

		// Close after random delay.
		time.Sleep(
			time.Duration(rapid.IntRange(0, 5).Draw(rt, "delay")) *
				time.Millisecond,
		)
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
	receiveCtx, receiveCancel := context.WithTimeout(
		ctx, 500*time.Millisecond,
	)
	defer receiveCancel()

	// Consume one iteration -- this will attempt to decode, fail, and
	// dead-letter since attempts >= max_attempts.
	for range mailbox.Receive(receiveCtx) {
		// Should not yield any valid envelope for the poison message.
		t.Fatal(
			"should not receive a valid envelope for poison " +
				"message",
		)
	}

	// Verify the poison message was dead-lettered.
	store.mu.Lock()
	numDL := len(store.deadLetters)
	numMessages := len(store.messages)
	store.mu.Unlock()

	require.Equal(
		t, 1, numDL, "poison message should be in dead letter queue",
	)
	require.Equal(
		t, 0, numMessages,
		"poison message should be removed from mailbox",
	)
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
	receiveCtx, receiveCancel := context.WithTimeout(
		ctx, 50*time.Millisecond,
	)
	defer receiveCancel()

	for range mailbox.Receive(receiveCtx) {
		t.Fatal(
			"should not receive a valid envelope for poison " +
				"message",
		)
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

	require.Equal(
		t, 0, numDL,
		"message should not be dead-lettered before max attempts",
	)
	require.Equal(
		t, 1, numMessages, "message should remain in mailbox for retry",
	)
	require.Greater(
		t, attempts, 0,
		"message should have been attempted at least once",
	)
	require.Less(
		t, attempts, maxAttempts,
		"message should not have exhausted max attempts",
	)
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
	enqueueErr := errors.New("simulated enqueue failure")
	store.mu.Lock()
	store.injectEnqueueError = enqueueErr
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

	// Send should return the enqueue failure.
	err := mailbox.Send(ctx, env)
	require.ErrorIs(t, err, enqueueErr)
	require.ErrorContains(t, err, "enqueue mailbox message")

	// The promise registry should be empty -- the entry should have been
	// cleaned up after the enqueue failure.
	mailbox.promiseRegistryMu.RLock()
	registrySize := len(mailbox.promiseRegistry)
	mailbox.promiseRegistryMu.RUnlock()

	require.Equal(
		t, 0, registrySize,
		"promise registry should be empty after enqueue failure",
	)

	// Verify that repeated failures don't accumulate stale entries.
	for range 10 {
		p := NewPromise[int]()
		env := envelope[*durableTestMsg, int]{
			message:   msg,
			promise:   p,
			callerCtx: ctx,
		}
		err := mailbox.Send(ctx, env)
		require.ErrorIs(t, err, enqueueErr)
	}

	mailbox.promiseRegistryMu.RLock()
	registrySize = len(mailbox.promiseRegistry)
	mailbox.promiseRegistryMu.RUnlock()

	require.Equal(
		t, 0, registrySize,
		"promise registry should remain empty after repeated failures",
	)
}

// TestDurableMailboxSendUsesOutboxIDFromContext verifies that when the context
// carries an outbox message ID (set by the OutboxPublisher),
// DurableMailbox.Send uses it as the inbox message ID instead of generating a
// fresh one. This enables receiver-side deduplication for CDC delivery retries.
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

	err := mailbox.Send(sendCtx, env)
	require.NoError(t, err)

	// Verify the stored message uses the outbox ID, not a fresh UUID.
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(t, store.messages, 1)

	storedMsg, exists := store.messages[outboxID]
	require.True(
		t, exists, "message should be stored with outbox ID as key",
	)
	require.Equal(t, outboxID, storedMsg.ID)
	require.Equal(t, "test-mailbox", storedMsg.MailboxID)
}

// TestDurableMailboxSendDuplicateOutboxIDIsIdempotent verifies that sending the
// same outbox-derived message ID twice is a no-op on the second attempt. This
// is the core receiver-side deduplication guarantee: if the OutboxPublisher
// retries after CompleteOutbox fails, the duplicate enqueue succeeds (returns
// true) without creating a second inbox message. (Fix #2 from Codex review.)
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
	err := mailbox.Send(sendCtx, env)
	require.NoError(t, err)

	// Second send with the same outbox ID should also succeed (idempotent).
	err = mailbox.Send(sendCtx, env)
	require.NoError(t, err)

	// Only one message should exist in the store.
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(
		t, store.messages, 1,
		"duplicate outbox ID should not create a second message",
	)
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
	err := mailbox.Send(ctx, env)
	require.NoError(t, err)

	// Verify a fresh UUIDv7 was generated (not empty, not a hardcoded
	// value).
	store.mu.Lock()
	defer store.mu.Unlock()

	require.Len(t, store.messages, 1)

	for id := range store.messages {
		require.NotEmpty(t, id)
		// UUIDv7 format: 8-4-4-4-12 hex chars with dashes.
		require.Len(
			t, id, 36, "generated ID should be a UUID (36 chars)",
		)
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

// TestDurableMailboxSendPreservesSenderTx verifies that Send preserves the
// sender's database transaction in the context passed to EnqueueMessage.
// This allows same-DB actors to share the transaction so the enqueue is
// atomic with the sender's state change.
func TestDurableMailboxSendPreservesSenderTx(t *testing.T) {
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
	err := mailbox.Send(sendCtx, env)
	require.NoError(t, err)

	// The context received by EnqueueMessage should carry the
	// sender's tx so same-DB actors share the transaction.
	require.NotNil(
		t, capturing.lastCtx, "EnqueueMessage should have been called",
	)
	require.True(
		t, HasTx(capturing.lastCtx),
		"EnqueueMessage context should carry the sender's tx",
	)
}

// TestNormalizePollIntervals verifies that the idle-poll floor and ceiling are
// resolved sanely from raw config values: unset fields take their defaults, and
// a ceiling below the floor is raised to the floor so the backoff can never
// produce a wait shorter than the configured poll interval.
func TestNormalizePollIntervals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		floor       time.Duration
		ceiling     time.Duration
		wantFloor   time.Duration
		wantCeiling time.Duration
	}{
		{
			name:        "both unset take defaults",
			wantFloor:   defaultPollInterval,
			wantCeiling: defaultMaxPollInterval,
		},
		{
			name:        "unset ceiling takes the default",
			floor:       5 * time.Second,
			wantFloor:   5 * time.Second,
			wantCeiling: defaultMaxPollInterval,
		},
		{
			name:        "unset floor takes the default",
			ceiling:     time.Minute,
			wantFloor:   defaultPollInterval,
			wantCeiling: time.Minute,
		},
		{
			name:        "negative values take defaults",
			floor:       -time.Second,
			ceiling:     -time.Minute,
			wantFloor:   defaultPollInterval,
			wantCeiling: defaultMaxPollInterval,
		},
		{
			// A ceiling under the floor would otherwise ask for a
			// wait that shrinks below what the operator configured.
			name:        "ceiling below floor is raised",
			floor:       10 * time.Second,
			ceiling:     time.Second,
			wantFloor:   10 * time.Second,
			wantCeiling: 10 * time.Second,
		},
		{
			// A floor above the default ceiling is the same
			// misconfiguration reached via the default, and must
			// not silently shrink the operator's poll interval.
			name:        "floor above the default ceiling wins",
			floor:       time.Hour,
			wantFloor:   time.Hour,
			wantCeiling: time.Hour,
		},
		{
			name:        "valid pair passes through",
			floor:       time.Second,
			ceiling:     30 * time.Second,
			wantFloor:   time.Second,
			wantCeiling: 30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			floor, ceiling := normalizePollIntervals(
				tc.floor, tc.ceiling,
			)
			require.Equal(t, tc.wantFloor, floor)
			require.Equal(t, tc.wantCeiling, ceiling)
		})
	}
}

// TestPollBackoff verifies the idle-poll backoff schedule: it starts at the
// floor, doubles on each empty poll, pins at the ceiling, and snaps back to the
// floor on reset.
func TestPollBackoff(t *testing.T) {
	t.Parallel()

	t.Run("doubles up to the ceiling", func(t *testing.T) {
		t.Parallel()

		backoff := newPollBackoff(time.Second, 30*time.Second)

		// The first idle wait is the floor; each empty poll doubles the
		// next one until the ceiling pins it. Note that 16s doubles to
		// 32s, which is clamped down to the 30s ceiling rather than
		// overshooting it.
		want := []time.Duration{
			time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
			30 * time.Second,
			30 * time.Second,
			30 * time.Second,
		}

		got := make([]time.Duration, 0, len(want))
		for range want {
			got = append(got, backoff.current())
			backoff.decay()
		}

		require.Equal(t, want, got)
	})

	t.Run("reset returns to the floor", func(t *testing.T) {
		t.Parallel()

		backoff := newPollBackoff(time.Second, 30*time.Second)

		// Decay all the way to the ceiling, then reset.
		for range 10 {
			backoff.decay()
		}
		require.Equal(t, 30*time.Second, backoff.current())

		backoff.reset()
		require.Equal(t, time.Second, backoff.current())

		// The schedule restarts from the floor rather than resuming
		// where it left off.
		backoff.decay()
		require.Equal(t, 2*time.Second, backoff.current())
	})

	t.Run("misconfigured ceiling disables decay", func(t *testing.T) {
		t.Parallel()

		// A ceiling below the floor is clamped up, which leaves a
		// constant-cadence poll at the floor -- never a shrinking one.
		backoff := newPollBackoff(50*time.Second, 10*time.Second)

		for range 5 {
			require.Equal(t, 50*time.Second, backoff.current())
			backoff.decay()
		}
	})

	t.Run("zero values take defaults", func(t *testing.T) {
		t.Parallel()

		backoff := newPollBackoff(0, 0)
		require.Equal(t, defaultPollInterval, backoff.current())

		for range 20 {
			backoff.decay()
		}
		require.Equal(t, defaultMaxPollInterval, backoff.current())
	})

	t.Run("always within bounds", func(t *testing.T) {
		t.Parallel()

		// Whatever sequence of decays and resets it sees, the backoff
		// never leaves [floor, ceiling] and never shrinks except on a
		// reset.
		rapid.Check(t, func(rt *rapid.T) {
			floorNanos := rapid.Int64Range(-1000, 1e6).Draw(
				rt, "floor",
			)
			ceilingNanos := rapid.Int64Range(-1000, 1e6).Draw(
				rt, "ceiling",
			)

			rawFloor := time.Duration(floorNanos)
			rawCeiling := time.Duration(ceilingNanos)

			backoff := newPollBackoff(rawFloor, rawCeiling)
			floor, ceiling := normalizePollIntervals(
				rawFloor, rawCeiling,
			)

			ops := rapid.SliceOfN(rapid.Bool(), 0, 64).Draw(
				rt, "ops",
			)

			for _, isDecay := range ops {
				prev := backoff.current()

				if isDecay {
					backoff.decay()
					require.GreaterOrEqual(
						rt, backoff.current(), prev,
					)
				} else {
					backoff.reset()
					require.Equal(
						rt, floor, backoff.current(),
					)
				}

				require.GreaterOrEqual(
					rt, backoff.current(), floor,
				)
				require.LessOrEqual(
					rt, backoff.current(), ceiling,
				)
			}
		})
	})
}

// TestDurableMailboxNormalizesPollIntervals verifies that a hand-built config
// that predates MaxPollInterval (or sets it nonsensically) still yields usable
// effective values on the constructed mailbox, so no construction path can end
// up with a zero-length poll wait that would tight-spin the store.
func TestDurableMailboxNormalizesPollIntervals(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newDurableTestCodec()
	ctx := context.Background()

	t.Run("unset fields take defaults", func(t *testing.T) {
		t.Parallel()

		mailbox := NewDurableMailbox[*durableTestMsg, int](
			ctx, DurableMailboxConfig{
				MailboxID: "unset",
				Store:     store,
				Codec:     codec,
			},
		)

		require.Equal(
			t, defaultPollInterval, mailbox.cfg.PollInterval,
		)
		require.Equal(
			t, defaultMaxPollInterval, mailbox.cfg.MaxPollInterval,
		)
	})

	t.Run("unset ceiling takes the default", func(t *testing.T) {
		t.Parallel()

		mailbox := NewDurableMailbox[*durableTestMsg, int](
			ctx, DurableMailboxConfig{
				MailboxID:    "legacy",
				Store:        store,
				Codec:        codec,
				PollInterval: 250 * time.Millisecond,
			},
		)

		require.Equal(
			t, 250*time.Millisecond, mailbox.cfg.PollInterval,
		)
		require.Equal(
			t, defaultMaxPollInterval, mailbox.cfg.MaxPollInterval,
		)
	})

	t.Run("ceiling below floor is raised", func(t *testing.T) {
		t.Parallel()

		mailbox := NewDurableMailbox[*durableTestMsg, int](
			ctx, DurableMailboxConfig{
				MailboxID:       "inverted",
				Store:           store,
				Codec:           codec,
				PollInterval:    10 * time.Second,
				MaxPollInterval: time.Second,
			},
		)

		require.Equal(t, 10*time.Second, mailbox.cfg.PollInterval)
		require.Equal(t, 10*time.Second, mailbox.cfg.MaxPollInterval)
	})
}

// pollRecorderStore wraps a mockDeliveryStore and timestamps every claim the
// receive loop makes. Tests assert on the observed sequence of claims -- their
// count and the gaps between them -- rather than on wall-clock sleeps, so the
// assertions describe the poll cadence the loop actually produced.
type pollRecorderStore struct {
	*mockDeliveryStore

	pollMu sync.Mutex
	polls  []time.Time
}

// newPollRecorderStore builds a recording store over a fresh mock.
func newPollRecorderStore() *pollRecorderStore {
	return &pollRecorderStore{
		mockDeliveryStore: newMockDeliveryStore(),
	}
}

// LeaseNextMessage records the moment of the claim before delegating to the
// underlying mock.
func (s *pollRecorderStore) LeaseNextMessage(ctx context.Context,
	mailboxID string, leaseToken string, leaseDuration time.Duration) (
	*LeasedMessage, error) {

	s.pollMu.Lock()
	s.polls = append(s.polls, time.Now())
	s.pollMu.Unlock()

	return s.mockDeliveryStore.LeaseNextMessage(
		ctx, mailboxID, leaseToken, leaseDuration,
	)
}

// pollCount returns how many claims the receive loop has made so far.
func (s *pollRecorderStore) pollCount() int {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()

	return len(s.polls)
}

// pollGaps returns the observed waits between consecutive claims.
func (s *pollRecorderStore) pollGaps() []time.Duration {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()

	gaps := make([]time.Duration, 0, len(s.polls))
	for i := 1; i < len(s.polls); i++ {
		gaps = append(gaps, s.polls[i].Sub(s.polls[i-1]))
	}

	return gaps
}

// runPollTestMailbox builds a mailbox with the given poll floor/ceiling over a
// recording store and drains its Receive iterator on a background goroutine.
// The returned channel carries every delivered message; the cleanup registered
// on t stops the loop and waits for it to exit.
func runPollTestMailbox(ctx context.Context, t *testing.T, mailboxID string,
	floor, ceiling time.Duration) (*pollRecorderStore,
	*DurableMailbox[*durableTestMsg, int], <-chan *durableTestMsg) {

	t.Helper()

	store := newPollRecorderStore()
	codec := newDurableTestCodec()

	cfg := DefaultDurableMailboxConfig(mailboxID, store, codec)
	cfg.PollInterval = floor
	cfg.MaxPollInterval = ceiling

	loopCtx, cancel := context.WithCancel(ctx)
	mailbox := NewDurableMailbox[*durableTestMsg, int](loopCtx, cfg)

	// The delivery channel is unbuffered so the test observes each delivery
	// at the moment the loop yields it.
	delivered := make(chan *durableTestMsg)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for env := range mailbox.Receive(loopCtx) {
			select {
			case delivered <- env.message:
			case <-loopCtx.Done():
				return
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return store, mailbox, delivered
}

// seedPollTestMessage enqueues a message straight into the store, bypassing
// Send and therefore firing no wake. This models the cross-process enqueue that
// only the fallback poll can discover: the store's RegisterMailboxWake callback
// is same-process, so a row committed by another replica arrives with no local
// signal at all.
func seedPollTestMessage(ctx context.Context, t *testing.T,
	store *pollRecorderStore, mailboxID, msgID string, value uint64) {

	t.Helper()

	codec := newDurableTestCodec()
	msg := &durableTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
	}
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	params := EnqueueParams{
		ID:          msgID,
		MailboxID:   mailboxID,
		MessageType: msg.MessageType(),
		Payload:     payload,
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 10,
	}
	require.NoError(t, store.EnqueueMessage(ctx, params))
}

// TestDurableMailboxIdlePollBackoffGrows verifies that consecutive empty polls
// widen the wait between claims along the doubling schedule and then pin at the
// ceiling. Every assertion is one-sided in the safe direction (a timer never
// fires early), so a loaded machine can only make the observed gaps longer,
// never shorter -- a fixed-cadence poll is what fails here, not a slow CI box.
func TestDurableMailboxIdlePollBackoffGrows(t *testing.T) {
	t.Parallel()

	const (
		floor   = 20 * time.Millisecond
		ceiling = 160 * time.Millisecond
	)

	// The scheduled waits after the loop's first (immediate) claim. Six
	// gaps is 620ms of idling on a correct implementation.
	want := []time.Duration{
		floor, 2 * floor, 4 * floor, ceiling, ceiling, ceiling,
	}

	store, _, _ := runPollTestMailbox(
		t.Context(), t, "decay-mailbox", floor, ceiling,
	)

	scheduleDone := func() bool {
		return store.pollCount() >= len(want)+1
	}
	require.Eventually(
		t, scheduleDone, 5*time.Second, 5*time.Millisecond,
		"receive loop did not complete the idle poll schedule",
	)

	// Allow a small slack purely for timestamp granularity: the failure
	// this guards against (no decay at all) is off by a full floor, which
	// dwarfs it.
	const slack = 2 * time.Millisecond

	gaps := store.pollGaps()
	require.GreaterOrEqual(t, len(gaps), len(want))

	for i, wantGap := range want {
		require.GreaterOrEqualf(
			t, gaps[i], wantGap-slack, "idle poll %d came after "+
				"%v, expected at least %v: the backoff did "+
				"not widen (gaps: %v)", i+1, gaps[i], wantGap,
			gaps[:len(want)],
		)
	}
}

// TestDurableMailboxIdlePollBackoffCaps verifies the ceiling actually bounds
// the wait. It asserts on elapsed work rather than on any single gap: an
// uncapped doubling schedule needs over twenty seconds to reach ten idle polls
// at this floor, while a capped one needs under half a second, so the budget
// here separates the two by an order of magnitude in both directions.
func TestDurableMailboxIdlePollBackoffCaps(t *testing.T) {
	t.Parallel()

	const (
		floor   = 20 * time.Millisecond
		ceiling = 40 * time.Millisecond

		// 20ms + 9 * 40ms = 380ms capped, versus 20 * (2^10 - 1) =
		// 20.4s uncapped.
		wantPolls = 11
	)

	store, _, _ := runPollTestMailbox(
		t.Context(), t, "cap-mailbox", floor, ceiling,
	)

	cappedPollsDone := func() bool {
		return store.pollCount() >= wantPolls
	}
	require.Eventually(
		t, cappedPollsDone, 3*time.Second, 5*time.Millisecond, "idle"+
			" polls did not cap at the ceiling: the backoff "+
			"kept doubling past MaxPollInterval",
	)

	// The loop must still have decayed on the way up to the ceiling.
	gaps := store.pollGaps()
	require.GreaterOrEqual(t, len(gaps), 2)
	require.GreaterOrEqual(t, gaps[1], 2*floor-2*time.Millisecond)
}

// TestDurableMailboxWakeResetsPollBackoff verifies that a wake signal snaps the
// idle backoff back to its floor. This is the property that keeps the decay off
// the delivery path: a same-process enqueue signals the wake channel (directly
// from Send, and again from the store's post-commit callback for an enqueue
// folded into a caller's transaction), so however far the backoff had decayed,
// the mailbox returns to its configured cadence immediately.
func TestDurableMailboxWakeResetsPollBackoff(t *testing.T) {
	t.Parallel()

	const (
		floor   = 20 * time.Millisecond
		ceiling = time.Second
	)

	store, mailbox, _ := runPollTestMailbox(
		t.Context(), t, "wake-reset-mailbox", floor, ceiling,
	)

	// Let the mailbox idle long enough for the backoff to widen well past
	// the floor (20+40+80+160 = 300ms of schedule elapses inside this
	// window, leaving the next wait at 320ms or more).
	time.Sleep(400 * time.Millisecond)

	base := store.pollCount()
	require.Positive(t, base)

	mailbox.Wake()

	// After the reset the next few polls run at the floor again: the wake's
	// own poll plus 20+40+80ms of schedule, about 140ms. Without the reset
	// the very next gap alone is already 320ms and the one after that
	// 640ms, so three more polls could not land inside this budget.
	backAtFloor := func() bool {
		return store.pollCount() >= base+4
	}
	require.Eventually(
		t, backAtFloor, time.Second, 5*time.Millisecond,
		"wake did not reset the idle poll backoff to its floor",
	)
}

// TestDurableMailboxClaimResetsPollBackoff verifies that successfully claiming
// a message snaps the backoff back to its floor, so a mailbox that goes busy
// after a long quiet stretch polls responsively from the very next gap. The
// message is seeded straight into the store with no wake, so the claim is the
// only thing that could have caused the reset.
func TestDurableMailboxClaimResetsPollBackoff(t *testing.T) {
	t.Parallel()

	const (
		floor   = 20 * time.Millisecond
		ceiling = time.Second
	)

	ctx := t.Context()
	store, _, delivered := runPollTestMailbox(
		ctx, t, "claim-reset-mailbox", floor, ceiling,
	)

	// Idle long enough that the backoff is several multiples of the floor.
	time.Sleep(400 * time.Millisecond)

	seedPollTestMessage(ctx, t, store, "claim-reset-mailbox", "msg-1", 42)

	select {
	case msg := <-delivered:
		require.Equal(t, uint64(42), msg.Value.Val)

	case <-time.After(5 * time.Second):
		t.Fatal("decayed poll never discovered the seeded message")
	}

	base := store.pollCount()

	// The claim resets the backoff, so the mailbox returns to floor cadence
	// even though it found nothing on the polls that follow (the claimed
	// message is leased out and no longer eligible). Without the reset the
	// backoff would still be at hundreds of milliseconds and climbing.
	backAtFloor := func() bool {
		return store.pollCount() >= base+4
	}
	require.Eventually(
		t, backAtFloor, time.Second, 5*time.Millisecond,
		"claiming a message did not reset the idle poll backoff",
	)
}

// TestDurableMailboxPollNeverStops verifies that the fallback poll survives an
// arbitrarily long idle stretch. Once the backoff is pinned at its ceiling the
// timer must keep firing, because it is the only discovery mechanism for a row
// enqueued by another process or replica: RegisterMailboxWake is a same-process
// callback, so a cross-process enqueue arrives with no local wake at all.
func TestDurableMailboxPollNeverStops(t *testing.T) {
	t.Parallel()

	const (
		floor   = 10 * time.Millisecond
		ceiling = 50 * time.Millisecond
	)

	ctx := t.Context()
	store, _, delivered := runPollTestMailbox(
		ctx, t, "never-stops-mailbox", floor, ceiling,
	)

	// Idle far past the point where the backoff is pinned at its ceiling.
	time.Sleep(500 * time.Millisecond)

	pinned := store.pollCount()
	require.Positive(t, pinned)

	// A cross-process enqueue: the row appears with no wake of any kind.
	seedPollTestMessage(ctx, t, store, "never-stops-mailbox", "msg-1", 7)

	select {
	case msg := <-delivered:
		require.Equal(t, uint64(7), msg.Value.Val)

	case <-time.After(5 * time.Second):
		t.Fatal(
			"poll timer stopped: a wakeless enqueue was never " +
				"discovered",
		)
	}

	// The discovery came from a poll that happened after the backoff was
	// already pinned, not from a leftover pre-idle tick.
	require.Greater(t, store.pollCount(), pinned)
}
