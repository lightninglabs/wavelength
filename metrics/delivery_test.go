package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestWithDeadLetterCounter verifies the wrapper counts exactly the deliveries
// the policy gives up on, and that it never changes the policy's decision.
func TestWithDeadLetterCounter(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[Msg]("test-metrics", 4)
	policy := WithDeadLetterCounter(
		func(_ error, attempts int) (bool, time.Duration) {
			return attempts < 2, time.Second
		},
		fn.Some[Sink](sink), ActorFamilyCreditOp,
	)

	// A retry decision emits nothing.
	retry, delay := policy(errors.New("boom"), 1)
	require.True(t, retry)
	require.Equal(t, time.Second, delay)
	require.Empty(t, sink.Messages())

	// The give-up decision is the dead-letter decision, and is counted for
	// the coarse actor family.
	retry, _ = policy(errors.New("boom"), 2)
	require.False(t, retry)

	msg, ok := sink.AwaitMessage(time.Second)
	require.True(t, ok)

	deadLetter, ok := msg.(*ActorDeadLetterMsg)
	require.True(t, ok)
	require.Equal(t, ActorFamilyCreditOp, deadLetter.Actor)
}

// TestWithDeadLetterCounterPassThrough verifies the wrapper is a no-op without
// a sink and that an unrecognized family cannot become its own label value.
func TestWithDeadLetterCounterPassThrough(t *testing.T) {
	t.Parallel()

	stop := func(error, int) (bool, time.Duration) {
		return false, 0
	}

	// No sink: the base policy is returned untouched.
	policy := WithDeadLetterCounter(
		stop, fn.None[Sink](), ActorFamilyCreditOp,
	)
	retry, _ := policy(errors.New("boom"), 1)
	require.False(t, retry)

	// A nil base policy falls back to the framework default.
	policy = WithDeadLetterCounter(nil, fn.None[Sink](), "")
	retry, _ = policy(errors.New("boom"), 0)
	require.True(t, retry)

	// An unrecognized family is bucketed, not passed through.
	sink := actor.NewChannelTellOnlyRef[Msg]("test-metrics", 1)
	policy = WithDeadLetterCounter(
		stop, fn.Some[Sink](sink), "credit-op-"+t.Name(),
	)
	_, _ = policy(errors.New("boom"), 1)

	msg, ok := sink.AwaitMessage(time.Second)
	require.True(t, ok)

	deadLetter, ok := msg.(*ActorDeadLetterMsg)
	require.True(t, ok)
	require.Equal(t, actorFamilyOther, deadLetter.Actor)
}
