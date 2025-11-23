package actor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testMessage is a simple message type for testing.
type testMessage struct {
	BaseMessage
	value int
}

func (m *testMessage) MessageType() string {
	return "testMessage"
}

// TestChannelMailboxSend tests that Send successfully delivers an envelope to
// the mailbox.
func TestChannelMailboxSend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	defer mailbox.Close()

	msg := &testMessage{value: 42}
	env := envelope[*testMessage, string]{
		message: msg,
		promise: nil,
	}

	// Send should succeed.
	ok := mailbox.Send(ctx, env)
	require.True(t, ok, "Send should succeed")

	// Verify the message can be received.
	for receivedEnv := range mailbox.Receive(ctx) {
		require.Equal(t, msg.value, receivedEnv.message.value)
		break
	}
}

// TestChannelMailboxSendContextCancelled tests that Send returns false when
// the caller's context is cancelled before the send completes.
func TestChannelMailboxSendContextCancelled(t *testing.T) {
	t.Parallel()

	actorCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a mailbox with capacity 0 (will default to 1) and fill it.
	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 1)
	defer mailbox.Close()

	// Fill the mailbox.
	env := envelope[*testMessage, string]{
		message: &testMessage{value: 1},
		promise: nil,
	}
	ok := mailbox.TrySend(env)
	require.True(t, ok, "First send should succeed")

	// Create a cancelled context and attempt to send. This should return
	// false immediately.
	cancelledCtx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()

	ok = mailbox.Send(cancelledCtx, envelope[*testMessage, string]{
		message: &testMessage{value: 2},
		promise: nil,
	})
	require.False(t, ok, "Send with cancelled context should fail")
}

// TestChannelMailboxSendToClosedMailbox tests that Send returns false when
// attempting to send to a closed mailbox.
func TestChannelMailboxSendToClosedMailbox(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	mailbox.Close()

	env := envelope[*testMessage, string]{
		message: &testMessage{value: 42},
		promise: nil,
	}

	// Send should fail because the mailbox is closed.
	ok := mailbox.Send(ctx, env)
	require.False(t, ok, "Send to closed mailbox should fail")
}

// TestChannelMailboxTrySend tests the non-blocking TrySend operation.
func TestChannelMailboxTrySend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 1)
	defer mailbox.Close()

	env1 := envelope[*testMessage, string]{
		message: &testMessage{value: 1},
		promise: nil,
	}

	// First TrySend should succeed.
	ok := mailbox.TrySend(env1)
	require.True(t, ok, "First TrySend should succeed")

	env2 := envelope[*testMessage, string]{
		message: &testMessage{value: 2},
		promise: nil,
	}

	// Second TrySend should fail because the mailbox is full.
	ok = mailbox.TrySend(env2)
	require.False(t, ok, "TrySend to full mailbox should fail")

	// Receive the first message.
	for receivedEnv := range mailbox.Receive(ctx) {
		require.Equal(t, 1, receivedEnv.message.value)
		break
	}

	// Now TrySend should succeed again.
	ok = mailbox.TrySend(env2)
	require.True(t, ok, "TrySend after receive should succeed")
}

// TestChannelMailboxTrySendToClosed tests that TrySend returns false when
// attempting to send to a closed mailbox.
func TestChannelMailboxTrySendToClosed(t *testing.T) {
	t.Parallel()

	actorCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	mailbox.Close()

	env := envelope[*testMessage, string]{
		message: &testMessage{value: 42},
		promise: nil,
	}

	// TrySend should fail because the mailbox is closed.
	ok := mailbox.TrySend(env)
	require.False(t, ok, "TrySend to closed mailbox should fail")
}

// TestChannelMailboxReceive tests that Receive yields envelopes from the
// mailbox using the iterator pattern.
func TestChannelMailboxReceive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	defer mailbox.Close()

	// Send multiple messages.
	numMessages := 5
	for i := 0; i < numMessages; i++ {
		env := envelope[*testMessage, string]{
			message: &testMessage{value: i},
			promise: nil,
		}
		ok := mailbox.Send(ctx, env)
		require.True(t, ok, "Send should succeed")
	}

	// Receive messages using the iterator.
	receivedCount := 0
	for env := range mailbox.Receive(ctx) {
		require.Equal(t, receivedCount, env.message.value)
		receivedCount++

		// Stop after receiving all messages.
		if receivedCount == numMessages {
			break
		}
	}

	require.Equal(t, numMessages, receivedCount,
		"Should receive all sent messages")
}

// TestChannelMailboxReceiveContextCancelled tests that Receive stops iteration
// when the context is cancelled.
func TestChannelMailboxReceiveContextCancelled(t *testing.T) {
	t.Parallel()

	actorCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	defer mailbox.Close()

	// Send a message.
	env := envelope[*testMessage, string]{
		message: &testMessage{value: 1},
		promise: nil,
	}
	ok := mailbox.Send(context.Background(), env)
	require.True(t, ok, "Send should succeed")

	// Create a context that will be cancelled.
	receiveCtx, receiveCancel := context.WithCancel(context.Background())

	// Start receiving in a goroutine.
	receivedCount := atomic.Int32{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for env := range mailbox.Receive(receiveCtx) {
			receivedCount.Add(1)
			require.Equal(t, 1, env.message.value)

			// Cancel the context after receiving the first message.
			receiveCancel()
		}
	}()

	// Wait for the goroutine to finish.
	select {
	case <-done:
		// Iteration stopped due to context cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not stop after context cancellation")
	}

	require.Equal(t, int32(1), receivedCount.Load(),
		"Should receive exactly one message")
}

// TestChannelMailboxCloseAndIsClosed tests the Close and IsClosed methods.
func TestChannelMailboxCloseAndIsClosed(t *testing.T) {
	t.Parallel()

	actorCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)

	// Initially not closed.
	require.False(t, mailbox.IsClosed(), "Mailbox should not be closed")

	// Close the mailbox.
	mailbox.Close()

	// Now it should be closed.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")

	// Calling Close again should be safe (idempotent).
	mailbox.Close()
	require.True(t, mailbox.IsClosed(), "Mailbox should still be closed")
}

// TestChannelMailboxDrain tests that Drain returns remaining envelopes after
// the mailbox is closed.
func TestChannelMailboxDrain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)

	// Send multiple messages.
	numMessages := 5
	for i := 0; i < numMessages; i++ {
		env := envelope[*testMessage, string]{
			message: &testMessage{value: i},
			promise: nil,
		}
		ok := mailbox.Send(ctx, env)
		require.True(t, ok, "Send should succeed")
	}

	// Close the mailbox without receiving the messages.
	mailbox.Close()

	// Drain should yield all the messages.
	drainedCount := 0
	for env := range mailbox.Drain() {
		require.Equal(t, drainedCount, env.message.value)
		drainedCount++
	}

	require.Equal(t, numMessages, drainedCount,
		"Drain should yield all remaining messages")
}

// TestChannelMailboxConcurrentSends tests that multiple goroutines can send to
// the mailbox concurrently without causing panics or data races.
func TestChannelMailboxConcurrentSends(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	numSenders := 10
	messagesPerSender := 100
	totalMessages := numSenders * messagesPerSender

	// Use a large enough mailbox to hold all messages without blocking.
	mailbox := NewChannelMailbox[*testMessage, string](
		actorCtx, totalMessages,
	)
	defer mailbox.Close()

	var wg sync.WaitGroup
	wg.Add(numSenders)

	// Launch multiple senders.
	for i := 0; i < numSenders; i++ {
		go func(senderID int) {
			defer wg.Done()

			for j := 0; j < messagesPerSender; j++ {
				env := envelope[*testMessage, string]{
					message: &testMessage{
						value: senderID*1000 + j,
					},
					promise: nil,
				}
				ok := mailbox.Send(ctx, env)
				require.True(t, ok, "Send should succeed")
			}
		}(i)
	}

	// Wait for all senders to finish.
	wg.Wait()

	// Verify that all messages were received.
	receivedCount := 0

	for range mailbox.Receive(ctx) {
		receivedCount++
		if receivedCount == totalMessages {
			break
		}
	}

	require.Equal(t, totalMessages, receivedCount,
		"Should receive all sent messages")
}

// TestChannelMailboxZeroCapacity tests that a mailbox with zero capacity
// defaults to capacity 1.
func TestChannelMailboxZeroCapacity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create a mailbox with zero capacity, which should default to 1.
	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 0)
	defer mailbox.Close()

	env := envelope[*testMessage, string]{
		message: &testMessage{value: 42},
		promise: nil,
	}

	// TrySend should succeed because the mailbox has at least capacity 1.
	ok := mailbox.TrySend(env)
	require.True(t, ok, "TrySend should succeed with default capacity")

	// Verify the message can be received.
	for receivedEnv := range mailbox.Receive(ctx) {
		require.Equal(t, 42, receivedEnv.message.value)
		break
	}
}

// TestChannelMailboxSendWithActorContextCancelled tests that Send returns
// false when the actor's context is cancelled.
func TestChannelMailboxSendWithActorContextCancelled(t *testing.T) {
	t.Parallel()

	actorCtx, actorCancel := context.WithCancel(context.Background())

	// Create a mailbox with capacity 1 and fill it.
	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 1)
	defer mailbox.Close()

	// Fill the mailbox.
	env1 := envelope[*testMessage, string]{
		message: &testMessage{value: 1},
		promise: nil,
	}
	ok := mailbox.TrySend(env1)
	require.True(t, ok, "First send should succeed")

	// Cancel the actor context.
	actorCancel()

	// Attempt to send another message. This should fail because the actor
	// context is cancelled.
	env2 := envelope[*testMessage, string]{
		message: &testMessage{value: 2},
		promise: nil,
	}
	ok = mailbox.Send(context.Background(), env2)
	require.False(t, ok, "Send should fail when actor context is cancelled")
}

// TestChannelMailboxReceiveStopsOnClose tests that the Receive iterator stops
// when the mailbox is closed.
func TestChannelMailboxReceiveStopsOnClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)

	// Send a few messages.
	for i := 0; i < 3; i++ {
		env := envelope[*testMessage, string]{
			message: &testMessage{value: i},
			promise: nil,
		}
		ok := mailbox.Send(ctx, env)
		require.True(t, ok, "Send should succeed")
	}

	// Start receiving in a goroutine.
	receivedCount := atomic.Int32{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for range mailbox.Receive(ctx) {
			receivedCount.Add(1)
		}
	}()

	// Give the receiver time to process messages.
	time.Sleep(100 * time.Millisecond)

	// Close the mailbox.
	mailbox.Close()

	// Wait for the receiver to finish.
	select {
	case <-done:
		// Iteration stopped after mailbox was closed.
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not stop after mailbox close")
	}

	require.Equal(t, int32(3), receivedCount.Load(),
		"Should receive all messages before close")
}

// TestActorDrainToDLO tests that when an actor is stopped, any unprocessed
// messages in the mailbox are drained and sent to the Dead Letter Office.
func TestActorDrainToDLO(t *testing.T) {
	t.Parallel()

	// Create a DLO to capture drained messages.
	dloMsgs := make(chan Message, 10)
	dloBehavior := NewFunctionBehavior(
		func(_ context.Context, msg Message) fn.Result[any] {
			dloMsgs <- msg
			return fn.Ok[any](nil)
		},
	)
	dloActor := NewActor(ActorConfig[Message, any]{
		ID:          "test-dlo",
		Behavior:    dloBehavior,
		MailboxSize: 10,
	})
	dloActor.Start()
	defer dloActor.Stop()

	// Create a blocking behavior that will hold the actor busy.
	blockCh := make(chan struct{})
	blockingBehavior := NewFunctionBehavior(
		func(ctx context.Context, _ *testMessage) fn.Result[string] {
			select {
			case <-blockCh:
				return fn.Ok("done")
			case <-ctx.Done():
				return fn.Err[string](ctx.Err())
			}
		},
	)

	// Create the main actor with the DLO configured.
	actor := NewActor(ActorConfig[*testMessage, string]{
		ID:          "test-actor",
		Behavior:    blockingBehavior,
		DLO:         dloActor.Ref(),
		MailboxSize: 10,
	})
	actor.Start()

	ctx := context.Background()

	// Send several messages. The first one will block the actor, causing
	// subsequent messages to queue up in the mailbox.
	numMessages := 5
	for i := 0; i < numMessages; i++ {
		actor.Ref().Tell(ctx, &testMessage{value: i})
	}

	// Give messages time to queue up.
	time.Sleep(50 * time.Millisecond)

	// Stop the actor while messages are still queued. This should drain the
	// queued messages to the DLO.
	actor.Stop()

	// The blocking message's context will be cancelled, and it won't be sent
	// to DLO since it was being processed. The remaining queued messages
	// should be drained to the DLO.
	receivedCount := 0
	timeout := time.After(2 * time.Second)
	for receivedCount < numMessages-1 {
		select {
		case msg := <-dloMsgs:
			testMsg, ok := msg.(*testMessage)
			require.True(t, ok, "DLO received unexpected message type")
			t.Logf("DLO received message with value: %d", testMsg.value)
			receivedCount++

		case <-timeout:
			t.Fatalf("Timed out waiting for DLO messages. "+
				"Received %d, expected %d",
				receivedCount, numMessages-1)
		}
	}

	// Verify we got the expected number of drained messages (all but the
	// first one which was being processed).
	require.Equal(t, numMessages-1, receivedCount,
		"DLO should receive all queued messages except the one "+
			"being processed")
}

// TestChannelMailboxWithPromises tests that envelopes with promises are
// handled correctly.
func TestChannelMailboxWithPromises(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	actorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mailbox := NewChannelMailbox[*testMessage, string](actorCtx, 10)
	defer mailbox.Close()

	// Create a promise for the response.
	promise := NewPromise[string]()

	env := envelope[*testMessage, string]{
		message: &testMessage{value: 42},
		promise: promise,
	}

	// Send the envelope with a promise.
	ok := mailbox.Send(ctx, env)
	require.True(t, ok, "Send should succeed")

	// Receive the envelope and complete the promise.
	for receivedEnv := range mailbox.Receive(ctx) {
		require.Equal(t, 42, receivedEnv.message.value)
		require.NotNil(t, receivedEnv.promise,
			"Envelope should contain promise")

		// Complete the promise.
		receivedEnv.promise.Complete(fn.Ok("response"))
		break
	}

	// Verify the promise was completed.
	future := promise.Future()
	result := future.Await(ctx)
	response, err := result.Unpack()
	require.NoError(t, err, "Promise should be completed successfully")
	require.Equal(t, "response", response)
}
