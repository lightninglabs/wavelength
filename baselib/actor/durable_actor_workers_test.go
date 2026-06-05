package actor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestDurableActorWorkerCountClamped verifies the NumWorkers config is clamped
// to at least one, so a zero or negative value preserves single-worker
// semantics, while the default config requests exactly one worker and an
// explicit count is honored.
func TestDurableActorWorkerCountClamped(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	behavior := newMockBehavior(fn.Ok(0))

	require.Equal(
		t, 1, DefaultDurableActorConfig(
			"a", behavior, store, codec,
		).NumWorkers,
	)

	for _, n := range []int{-3, 0, 1} {
		cfg := DefaultDurableActorConfig("a", behavior, store, codec)
		cfg.NumWorkers = n
		actor := NewDurableActor(cfg).UnwrapOrFail(t)
		require.Equal(t, 1, actor.numWorkers)
	}

	cfg := DefaultDurableActorConfig("a", behavior, store, codec)
	cfg.NumWorkers = 4
	actor := NewDurableActor(cfg).UnwrapOrFail(t)
	require.Equal(t, 4, actor.numWorkers)
}

// TestDurableActorNumWorkersProcessesEachMessageOnce verifies that a
// multi-worker durable actor drains its single mailbox as a competing-consumer
// pool: every enqueued message is processed exactly once (the lease prevents a
// double-claim) and independent messages run concurrently across the workers.
func TestDurableActorNumWorkersProcessesEachMessageOnce(t *testing.T) {
	t.Parallel()

	const (
		numWorkers = 4
		numMsgs    = 24
	)

	var (
		inFlight    atomic.Int64
		maxInFlight atomic.Int64
		seenMu      sync.Mutex
		seen        = make(map[uint64]int)
	)

	behavior := newMockBehavior(fn.Ok(0))
	behavior.onReceive = func(_ context.Context, msg *actorTestMsg) {
		// Bracket the handler so concurrent siblings are observable.
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}

		time.Sleep(15 * time.Millisecond)

		seenMu.Lock()
		seen[msg.Value.Val]++
		seenMu.Unlock()

		inFlight.Add(-1)
	}

	store := newMockDeliveryStore()
	codec := newActorTestCodec()
	cfg := DefaultDurableActorConfig("pool-actor", behavior, store, codec)
	cfg.NumWorkers = numWorkers
	cfg.PollInterval = 5 * time.Millisecond

	actor := NewDurableActor(cfg).UnwrapOrFail(t)
	actor.Start()
	defer actor.Stop()

	ctx := context.Background()
	for i := 0; i < numMsgs; i++ {
		msg := &actorTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(i)),
			Payload: tlv.NewPrimitiveRecord[tlv.TlvType2](
				[]byte("m"),
			),
		}
		require.NoError(t, actor.Ref().Tell(ctx, msg))
	}

	// Wait on the post-handler record (not callCount, which increments
	// before the handler's work) so every handler has fully completed.
	require.Eventually(t, func() bool {
		seenMu.Lock()
		defer seenMu.Unlock()

		return len(seen) == numMsgs
	}, 5*time.Second, 10*time.Millisecond)

	// Every message was processed exactly once.
	seenMu.Lock()
	defer seenMu.Unlock()
	require.Len(t, seen, numMsgs)
	for val, count := range seen {
		require.Equalf(
			t, 1, count, "message %d processed %d times", val,
			count,
		)
	}

	// The pool actually ran handlers concurrently.
	require.GreaterOrEqual(
		t, maxInFlight.Load(), int64(2),
		"expected concurrent processing across workers",
	)
}

// TestDurableActorSingleWorkerStaysSequential verifies that the default
// single-worker actor preserves the strictly-sequential per-actor processing
// other behaviors rely on: no two handlers ever run at once.
func TestDurableActorSingleWorkerStaysSequential(t *testing.T) {
	t.Parallel()

	var inFlight, maxInFlight atomic.Int64

	behavior := newMockBehavior(fn.Ok(0))
	behavior.onReceive = func(_ context.Context, _ *actorTestMsg) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}

		time.Sleep(5 * time.Millisecond)

		inFlight.Add(-1)
	}

	store := newMockDeliveryStore()
	codec := newActorTestCodec()

	// NumWorkers is left at the default of 1.
	cfg := DefaultDurableActorConfig("seq-actor", behavior, store, codec)
	cfg.PollInterval = 2 * time.Millisecond

	actor := NewDurableActor(cfg).UnwrapOrFail(t)
	actor.Start()
	defer actor.Stop()

	ctx := context.Background()
	const numMsgs = 12
	for i := 0; i < numMsgs; i++ {
		require.NoError(
			t,
			actor.Ref().Tell(ctx, &actorTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
					uint64(i),
				),
				Payload: tlv.NewPrimitiveRecord[tlv.TlvType2](
					[]byte("s"),
				),
			},
			),
		)
	}

	require.Eventually(t, func() bool {
		return behavior.callCount() == numMsgs
	}, 5*time.Second, 5*time.Millisecond)

	require.Equal(
		t, int64(1), maxInFlight.Load(),
		"single-worker actor must process sequentially",
	)
}

// TestDurableActorWorkerPoolExactlyOnceProperty is the property form of the
// competing-consumer invariant: for any worker count and any number of distinct
// messages, the pool processes each message exactly once. This stresses the
// lease claim under randomized concurrency that fixed examples cannot cover.
func TestDurableActorWorkerPoolExactlyOnceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		numWorkers := rapid.IntRange(1, 8).Draw(rt, "numWorkers")
		numMsgs := rapid.IntRange(1, 30).Draw(rt, "numMsgs")

		var mu sync.Mutex
		seen := make(map[uint64]int)

		behavior := newMockBehavior(fn.Ok(0))
		behavior.onReceive = func(_ context.Context, msg *actorTestMsg) {
			mu.Lock()
			seen[msg.Value.Val]++
			mu.Unlock()
		}

		store := newMockDeliveryStore()
		codec := newActorTestCodec()
		cfg := DefaultDurableActorConfig(
			"prop-actor", behavior, store, codec,
		)
		cfg.NumWorkers = numWorkers
		cfg.PollInterval = 2 * time.Millisecond

		result := NewDurableActor(cfg)
		actor, err := result.Unpack()
		require.NoError(rt, err)

		actor.Start()
		defer actor.Stop()

		ctx := context.Background()
		for i := 0; i < numMsgs; i++ {
			msg := &actorTestMsg{
				Value: tlv.NewPrimitiveRecord[tlv.TlvType1](
					uint64(i),
				),
				Payload: tlv.NewPrimitiveRecord[tlv.TlvType2](
					[]byte("p"),
				),
			}
			require.NoError(rt, actor.Ref().Tell(ctx, msg))
		}

		require.Eventually(rt, func() bool {
			mu.Lock()
			defer mu.Unlock()

			return len(seen) == numMsgs
		}, 5*time.Second, 2*time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		require.Len(rt, seen, numMsgs)
		for val, count := range seen {
			require.Equalf(
				rt, 1, count, "message %d processed %d times",
				val, count,
			)
		}
	})
}
