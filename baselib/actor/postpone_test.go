package actor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestPostponeDelayMatchesWrapChain pins the detection contract: a
// PostponeError is recognized bare, wrapped, and via errors.Is on the
// sentinel, and an ordinary error is not.
func TestPostponeDelayMatchesWrapChain(t *testing.T) {
	t.Parallel()

	bare := Postpone(5 * time.Second)
	delay, ok := postponeDelay(bare)
	require.True(t, ok)
	require.Equal(t, 5*time.Second, delay)
	require.ErrorIs(t, bare, ErrPostponed)

	wrapped := fmt.Errorf("over cap: %w", Postpone(time.Minute))
	delay, ok = postponeDelay(wrapped)
	require.True(t, ok)
	require.Equal(t, time.Minute, delay)

	_, ok = postponeDelay(errors.New("real failure"))
	require.False(t, ok)
}

// TestDeliveryPostponeLeased verifies the fenced delivery-level postpone:
// the lease clears, the attempt the lease burned is restored, and a second
// postpone on the same delivery reports ErrAlreadyAcked.
func TestDeliveryPostponeLeased(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	require.NoError(
		t,
		store.EnqueueMessage(
			ctx, EnqueueParams{
				ID:          "pp-1",
				MailboxID:   "mb",
				MessageType: "t",
				Payload:     []byte{1},
				MaxAttempts: 3,
			},
		),
	)

	leased, err := store.LeaseNextMessage(ctx, "mb", "tok", time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, leased.Attempts)

	d := &Delivery[*durableTestMsg, int]{
		ID:         "pp-1",
		LeaseToken: "tok",
		Attempts:   leased.Attempts,
		store:      store,
	}

	require.NoError(t, d.Postpone(ctx, time.Second))

	store.mu.Lock()
	msg := store.messages["pp-1"]
	require.Zero(t, msg.Attempts)
	require.Empty(t, msg.LeaseToken)
	store.mu.Unlock()

	require.ErrorIs(t, d.Postpone(ctx, time.Second), ErrAlreadyAcked)
}

// TestDeliveryPostponeLeaseless verifies the unfenced postpone leaves the
// attempts budget of a peeked (empty-token) delivery untouched.
func TestDeliveryPostponeLeaseless(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	ctx := context.Background()

	require.NoError(
		t,
		store.EnqueueMessage(
			ctx, EnqueueParams{
				ID:          "pp-2",
				MailboxID:   "mb",
				MessageType: "t",
				Payload:     []byte{1},
				MaxAttempts: 3,
			},
		),
	)

	d := &Delivery[*durableTestMsg, int]{
		ID:        "pp-2",
		leaseless: true,
		store:     store,
	}

	require.NoError(t, d.Postpone(ctx, time.Second))

	store.mu.Lock()
	require.Zero(t, store.messages["pp-2"].Attempts)
	store.mu.Unlock()
}

// TestDurableActorPostponeDoesNotBurnAttempts drives a full actor: the
// behavior postpones the first two deliveries and succeeds on the third. The
// message must redeliver past the point where the retry policy would have
// dead-lettered a nacked message, the retry policy must never be consulted,
// and the message must end processed rather than dead-lettered.
func TestDurableActorPostponeDoesNotBurnAttempts(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()

	var deliveries atomic.Int32
	behavior := newMockBehavior(fn.Err[int](Postpone(time.Millisecond)))
	behavior.onReceive = func(ctx context.Context, msg *actorTestMsg) {
		// Succeed on the third delivery. A nack-based retry with the
		// policy below would have dead-lettered after the first.
		if deliveries.Add(1) >= 3 {
			behavior.setResult(fn.Ok(42))
		}
	}

	cfg := DefaultDurableActorConfig("test-actor", behavior, store, codec)
	cfg.PollInterval = 10 * time.Millisecond

	// A policy that never retries AND fails the test if consulted with a
	// postpone: postpones are control flow and must bypass it entirely.
	cfg.TellRetryPolicy = func(err error, attempts int) (bool,
		time.Duration) {

		if _, ok := postponeDelay(err); ok {
			t.Errorf("retry policy consulted for a postpone")
		}

		return false, 0
	}

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()
	defer a.Stop()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(7)),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), msg))

	// The message survives two postponed deliveries and completes on the
	// third: consumed from the mailbox with nothing dead-lettered.
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()

		return len(store.messages) == 0 && len(store.deadLetters) == 0
	}, 5*time.Second, 10*time.Millisecond)

	require.GreaterOrEqual(t, deliveries.Load(), int32(3))

	// The attempts budget was never burned: every redelivery re-leased at
	// attempt 1, so the message completed normally -- a dedup entry
	// exists and no dead letter was written, even though a nack-based
	// loop under this never-retry policy would have dead-lettered on the
	// first failure.
	store.mu.Lock()
	require.Empty(t, store.deadLetters)
	require.NotEmpty(t, store.processed)
	store.mu.Unlock()
}
