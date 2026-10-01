package actor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// cycleMsg is the message type of the cyclic actors below.
type cycleMsg struct {
	BaseMessage
	kind string
	seq  int
}

// MessageType returns the message type name.
func (m *cycleMsg) MessageType() string {
	return "cycleMsg"
}

// cycleA answers queries, and on "start" Tells peer count messages using its
// turn context.
type cycleA struct {
	peer  ActorRef[*cycleMsg, string]
	count int
}

// Receive implements ActorBehavior.
func (a *cycleA) Receive(ctx context.Context, msg *cycleMsg) fn.Result[string] {
	if msg.kind != "start" {
		return fn.Ok("pong")
	}

	for i := 0; i < a.count; i++ {
		err := a.peer.Tell(ctx, &cycleMsg{kind: "work", seq: i})
		if err != nil {
			return fn.Err[string](err)
		}
	}

	return fn.Ok("started")
}

// cycleB asks A a question for every work message it receives.
type cycleB struct {
	asker ActorRef[*cycleMsg, string]

	mu      sync.Mutex
	results []error
	done    chan struct{}
	want    int
}

// Receive implements ActorBehavior.
func (b *cycleB) Receive(ctx context.Context, msg *cycleMsg) fn.Result[string] {
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	_, err := b.asker.Ask(
		callCtx, &cycleMsg{kind: "query"},
	).Await(callCtx).Unpack()

	b.mu.Lock()
	defer b.mu.Unlock()

	b.results = append(b.results, err)
	if len(b.results) == b.want {
		close(b.done)
	}

	return fn.Ok("done")
}

// TestInTurnTellDoesNotDeadlockAskCycle covers the deadlock this package used
// to allow. Actor A, while handling one message, Tells actor B many messages.
// B's mailbox holds one, so A's blocking Tell would park A's receive loop. B,
// for each message, Asks A and waits for the reply, but A can never serve the
// Ask while parked. Each side waits on the other forever. With in-turn sends
// that never park, A finishes its loop, serves B's queries, and every
// message is processed.
func TestInTurnTellDoesNotDeadlockAskCycle(t *testing.T) {
	t.Parallel()

	const count = 50

	behA := &cycleA{count: count}
	actorA := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "cycle-a", Behavior: behA, MailboxSize: 1,
	})

	behB := &cycleB{done: make(chan struct{}), want: count}
	actorB := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "cycle-b", Behavior: behB, MailboxSize: 1,
	})

	behA.peer = actorB.Ref()
	behB.asker = actorA.Ref()

	actorA.Start()
	actorB.Start()
	t.Cleanup(actorA.Stop)
	t.Cleanup(actorB.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, actorA.Ref().Tell(ctx, &cycleMsg{kind: "start"}))

	select {
	case <-behB.done:
	case <-ctx.Done():
		t.Fatal("actors deadlocked")
	}

	behB.mu.Lock()
	defer behB.mu.Unlock()

	for i, err := range behB.results {
		require.NoError(t, err, "message %d", i)
	}
}

// durableTeller is a durable behavior that Tells a channel mailbox actor with
// its turn context and returns any send error as its own result.
type durableTeller struct {
	target TellOnlyRef[*cycleMsg]

	mu   sync.Mutex
	errs []error
}

// Receive implements ActorBehavior.
func (d *durableTeller) Receive(ctx context.Context,
	msg *actorTestMsg) fn.Result[int] {

	err := d.target.Tell(ctx, &cycleMsg{kind: "work", seq: 1000})

	d.mu.Lock()
	d.errs = append(d.errs, err)
	d.mu.Unlock()

	if err != nil {
		return fn.Err[int](err)
	}

	return fn.Ok(1)
}

// results returns a copy of the send results recorded so far.
func (d *durableTeller) results() []error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]error(nil), d.errs...)
}

// TestDurableTellAtOverflowCapGetsErrorAndRetries covers the durable sender
// case. A durable behavior Tells a channel mailbox whose overflow is at its
// cap. The Tell must come back as an ordinary error the behavior returns,
// which a durable actor handles with nack and retry. A panic would be
// recovered by the durable actor and never reach the behavior's result. Once
// the target drains below the cap the redelivery succeeds.
func TestDurableTellAtOverflowCapGetsErrorAndRetries(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	gate := make(chan struct{})
	beh := &gatedBehavior{started: started, gate: gate}

	// The gated message occupies the behavior, one fills the channel and
	// two fill the overflow, so the cap of two is reached.
	target := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "durable-target", Behavior: beh, MailboxSize: 1,
		MailboxOverflowLimit: 2,
	})
	target.Start()
	t.Cleanup(target.Stop)

	fillCtx, endFill := WithTurnForTest(context.Background(), "filler")
	defer endFill()

	require.NoError(t, target.Ref().Tell(fillCtx, &cycleMsg{seq: 0}))
	<-started
	for i := 1; i < 4; i++ {
		require.NoError(
			t,
			target.Ref().Tell(fillCtx, &cycleMsg{
				seq: i,
			},
			),
		)
	}

	teller := &durableTeller{target: target.Ref()}
	store := newMockDeliveryStore()
	cfg := DefaultDurableActorConfig(
		"durable-sender", teller, store, newActorTestCodec(),
	)
	cfg.PollInterval = 10 * time.Millisecond

	// The mock store ignores the retry delay, so redeliveries come back to
	// back. Keep the attempt budget well above what the test can burn.
	cfg.MaxAttempts = 1_000_000
	cfg.TellRetryPolicy = func(err error, attempts int) (bool,
		time.Duration) {

		return true, 10 * time.Millisecond
	}

	sender := NewDurableActor(cfg).UnwrapOrFail(t)
	sender.Start()
	t.Cleanup(sender.Stop)

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(t, sender.Ref().Tell(context.Background(), msg))

	// The behavior sees the overflow error as a normal send result, and
	// the delivery is retried while the target stays full.
	require.Eventually(t, func() bool {
		errs := teller.results()

		return len(errs) >= 2 &&
			errors.Is(errs[0], ErrMailboxOverflow) &&
			errors.Is(errs[1], ErrMailboxOverflow)
	}, 10*time.Second, 5*time.Millisecond)

	// Let the target drain. A later redelivery then succeeds.
	close(gate)

	require.Eventually(t, func() bool {
		errs := teller.results()

		return len(errs) > 0 && errs[len(errs)-1] == nil
	}, 10*time.Second, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		beh.mu.Lock()
		defer beh.mu.Unlock()

		return len(beh.seen) == 5 && beh.seen[4] == 1000
	}, 10*time.Second, 5*time.Millisecond)

	beh.mu.Lock()
	defer beh.mu.Unlock()

	require.Equal(t, []int{0, 1, 2, 3, 1000}, beh.seen)
}
