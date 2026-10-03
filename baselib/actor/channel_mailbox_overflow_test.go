package actor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// overflowEnv builds a plain envelope carrying the given value.
func overflowEnv(v int) envelope[*testMessage, string] {
	return envelope[*testMessage, string]{
		message: &testMessage{
			value: v,
		},
	}
}

// newOverflowMailbox creates a mailbox whose actor context is cancelled when
// the test ends.
func newOverflowMailbox(t *testing.T, capacity int,
	opts ...ChannelMailboxOption) *ChannelMailbox[*testMessage, string] {

	t.Helper()

	actorCtx, cancel := context.WithCancel(context.Background())

	mb := NewChannelMailbox[*testMessage, string](
		actorCtx, capacity, opts...,
	)

	// Cleanups run LIFO. Register Close first so cancel runs before it,
	// mirroring the actor loop: cancelling releases any external sender
	// still parked on the channel, which would otherwise hold the read
	// lock and block Close forever on a failing run.
	t.Cleanup(mb.Close)
	t.Cleanup(cancel)

	return mb
}

// receiveN pulls n envelopes off the mailbox and returns their values.
func receiveN(t *testing.T, mb *ChannelMailbox[*testMessage, string],
	n int) []int {

	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	vals := make([]int, 0, n)
	for env := range mb.Receive(ctx) {
		vals = append(vals, env.message.value)
		if len(vals) == n {
			break
		}
	}
	require.Len(t, vals, n)

	return vals
}

// TestMailboxInTurnSendNeverBlocks verifies that a send made with a turn
// context into a full mailbox returns at once and that a single sender's
// order survives the trip through the channel and the overflow.
func TestMailboxInTurnSendNeverBlocks(t *testing.T) {
	t.Parallel()

	mb := newOverflowMailbox(t, 2)
	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	const total = 20
	done := make(chan struct{})
	go func() {
		defer close(done)

		for i := 1; i <= total; i++ {
			require.NoError(t, mb.Send(turnCtx, overflowEnv(i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-turn send blocked on a full mailbox")
	}

	require.Equal(t, total-2, mb.OverflowLen())

	vals := receiveN(t, mb, total)
	for i, v := range vals {
		require.Equal(t, i+1, v)
	}
	require.Zero(t, mb.OverflowLen())
}

// TestMailboxNonTurnSendStillBlocks verifies that producers outside an active
// turn keep the blocking backpressure, including one holding a context from a
// turn that has already ended.
func TestMailboxNonTurnSendStillBlocks(t *testing.T) {
	t.Parallel()

	mb := newOverflowMailbox(t, 1)
	require.NoError(t, mb.Send(context.Background(), overflowEnv(0)))

	endedCtx, end := WithTurnForTest(context.Background(), "gone")
	end()

	for name, base := range map[string]context.Context{
		"plain":      context.Background(),
		"ended turn": endedCtx,
	} {
		ctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
		err := mb.Send(ctx, overflowEnv(1))
		cancel()

		require.ErrorIs(t, err, context.DeadlineExceeded, name)
	}

	require.Zero(t, mb.OverflowLen())
}

// TestMailboxTrySendWhileOverflowed verifies that TrySend reports a full
// mailbox while the overflow holds envelopes, even if the channel has room,
// so it cannot overtake an earlier in-turn send.
func TestMailboxTrySendWhileOverflowed(t *testing.T) {
	t.Parallel()

	mb := newOverflowMailbox(t, 1)
	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	require.NoError(t, mb.Send(turnCtx, overflowEnv(1)))
	require.NoError(t, mb.Send(turnCtx, overflowEnv(2)))
	require.Equal(t, 1, mb.OverflowLen())
	require.ErrorIs(t, mb.TrySend(overflowEnv(3)), ErrMailboxFull)

	// Receiving 1 refills the channel with 2, which empties the overflow.
	require.Equal(t, []int{1}, receiveN(t, mb, 1))
	require.Zero(t, mb.OverflowLen())
	require.ErrorIs(t, mb.TrySend(overflowEnv(3)), ErrMailboxFull)

	require.Equal(t, []int{2}, receiveN(t, mb, 1))
	require.NoError(t, mb.TrySend(overflowEnv(3)))
}

// TestMailboxDrainYieldsOverflow verifies that after Close, Drain yields the
// channel contents followed by the overflow, in send order.
func TestMailboxDrainYieldsOverflow(t *testing.T) {
	t.Parallel()

	mb := newOverflowMailbox(t, 2)
	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	for i := 1; i <= 7; i++ {
		require.NoError(t, mb.Send(turnCtx, overflowEnv(i)))
	}

	// Two fit in the channel, the rest trail it in the overflow.
	require.Equal(t, 5, mb.OverflowLen())

	// Drain does nothing until the mailbox is closed.
	for range mb.Drain() {
		t.Fatal("drained an open mailbox")
	}

	mb.Close()
	require.ErrorIs(
		t,
		mb.Send(
			turnCtx, overflowEnv(8),
		),
		ErrMailboxClosed,
	)

	var vals []int
	for env := range mb.Drain() {
		vals = append(vals, env.message.value)
	}
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, vals)
	require.Zero(t, mb.OverflowLen())
}

// TestMailboxOverflowLimitRejects verifies that exceeding the hard cap returns
// ErrMailboxOverflow instead of panicking, leaves the queue untouched, keeps
// delivering what was queued in order, and accepts in-turn sends again once
// the receiver has drained below the cap. It replaces the earlier test that
// asserted a panic at the cap.
func TestMailboxOverflowLimitRejects(t *testing.T) {
	t.Parallel()

	mb := newOverflowMailbox(
		t, 1, WithOverflowLimit(3), WithMailboxID("limited"),
	)
	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	// One envelope fills the channel and three fill the overflow.
	for i := 0; i < 4; i++ {
		require.NoError(t, mb.Send(turnCtx, overflowEnv(i)))
	}

	// Every send at the cap is rejected, and none of them is queued.
	for i := 0; i < 3; i++ {
		var err error
		require.NotPanics(t, func() {
			err = mb.Send(turnCtx, overflowEnv(99))
		})
		require.ErrorIs(t, err, ErrMailboxOverflow)
		require.ErrorContains(t, err, "limited")
		require.Equal(t, 3, mb.OverflowLen())
	}

	// The queued messages arrive in send order, with no rejected message
	// among them. Taking one off the channel refills it from the overflow,
	// which frees a slot below the cap.
	require.Equal(t, []int{0}, receiveN(t, mb, 1))
	require.Equal(t, 2, mb.OverflowLen())

	require.NoError(t, mb.Send(turnCtx, overflowEnv(4)))
	require.Equal(t, 3, mb.OverflowLen())
	require.ErrorIs(
		t,
		mb.Send(
			turnCtx, overflowEnv(98),
		),
		ErrMailboxOverflow,
	)

	require.Equal(t, []int{1, 2, 3, 4}, receiveN(t, mb, 4))
	require.Zero(t, mb.OverflowLen())
}

// TestMailboxOverflowRejectionIsClean verifies a rejected send through the
// actor references leaves nothing behind: a Tell returns the error, and an Ask
// completes its future with the error instead of leaving it pending.
func TestMailboxOverflowRejectionIsClean(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	gate := make(chan struct{})
	beh := &gatedBehavior{started: started, gate: gate}

	target := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "capped", Behavior: beh, MailboxSize: 1,
		MailboxOverflowLimit: 2,
	})
	target.Start()
	t.Cleanup(target.Stop)
	t.Cleanup(func() { close(gate) })

	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	// The first message occupies the behavior, the second fills the
	// channel and two more fill the overflow.
	require.NoError(t, target.Ref().Tell(
		turnCtx, &cycleMsg{seq: 0},
	))
	<-started
	for i := 1; i < 4; i++ {
		require.NoError(
			t,
			target.Ref().Tell(
				turnCtx, &cycleMsg{
					seq: i,
				},
			),
		)
	}

	err := target.Ref().Tell(turnCtx, &cycleMsg{seq: 99})
	require.ErrorIs(t, err, ErrMailboxOverflow)

	// The Ask future is already complete with the error, so awaiting it
	// with an expired context still returns the send error.
	fut := target.Ref().Ask(turnCtx, &cycleMsg{seq: 100})
	awaitCtx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	_, err = fut.Await(awaitCtx).Unpack()
	require.ErrorIs(t, err, ErrMailboxOverflow)
}

// gatedBehavior blocks its first message until gate is closed, and records the
// sequence numbers it sees.
type gatedBehavior struct {
	started chan struct{}
	gate    chan struct{}

	once sync.Once
	mu   sync.Mutex
	seen []int
}

// Receive implements ActorBehavior.
func (b *gatedBehavior) Receive(ctx context.Context,
	msg *cycleMsg) fn.Result[string] {

	b.once.Do(func() {
		close(b.started)
		<-b.gate
	})

	b.mu.Lock()
	b.seen = append(b.seen, msg.seq)
	b.mu.Unlock()

	return fn.Ok("ok")
}

// TestMailboxOverflowConcurrent runs in-turn senders, external senders and a
// single receiver together. No envelope may be lost, and each sender's
// envelopes must arrive in the order that sender sent them.
func TestMailboxOverflowConcurrent(t *testing.T) {
	t.Parallel()

	const (
		turnSenders = 4
		extSenders  = 3
		perSender   = 500
		total       = (turnSenders + extSenders) * perSender
	)

	mb := newOverflowMailbox(t, 4)

	var wg sync.WaitGroup
	send := func(sender int, ctx context.Context) {
		defer wg.Done()

		for seq := 0; seq < perSender; seq++ {
			// Encode the sender and its sequence number.
			err := mb.Send(ctx, overflowEnv(sender*perSender+seq))
			require.NoError(t, err)
		}
	}

	for i := 0; i < turnSenders; i++ {
		ctx, end := WithTurnForTest(
			context.Background(), fmt.Sprintf("turn-%d", i),
		)
		defer end()

		wg.Add(1)
		go send(i, ctx)
	}
	for i := 0; i < extSenders; i++ {
		wg.Add(1)
		go send(turnSenders+i, context.Background())
	}

	vals := receiveN(t, mb, total)
	wg.Wait()

	next := make(map[int]int)
	for _, v := range vals {
		sender, seq := v/perSender, v%perSender
		require.Equal(t, next[sender], seq, "sender %d", sender)
		next[sender]++
	}
	require.Len(t, next, turnSenders+extSenders)
	require.Zero(t, mb.OverflowLen())
}

// TestMailboxOverflowRefillOrder streams a long run of in-turn sends through a
// one-slot channel against a concurrent receiver and asserts every envelope
// arrives once and in order. Most sends land before the receiver catches up,
// so this mainly exercises the refill path; the receive/append race window is
// covered by TestMailboxOverflowConcurrent, where parked external senders
// overlap the refill.
func TestMailboxOverflowRefillOrder(t *testing.T) {
	t.Parallel()

	const rounds = 3000

	mb := newOverflowMailbox(t, 1)
	turnCtx, end := WithTurnForTest(context.Background(), "sender")
	defer end()

	go func() {
		for i := 0; i < rounds; i++ {
			_ = mb.Send(turnCtx, overflowEnv(i))
		}
	}()

	vals := receiveN(t, mb, rounds)
	for i, v := range vals {
		require.Equal(t, i, v)
	}
}
