package metrics

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestMetricsActorReceive verifies that each metric message type the
// actor handles increments exactly the expected counter and label, and
// that an unknown message type is ignored without panicking.
func TestMetricsActorReceive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  Msg

		// counter is the collector the message should increment.
		counter func() float64
	}{
		{
			name: "round joined",
			msg:  &RoundJoinedMsg{},
			counter: func() float64 {
				return testutil.ToFloat64(RoundsJoinedTotal)
			},
		},
		{
			name: "round completed confirmed",
			msg: &RoundCompletedMsg{
				Status: "confirmed",
			},
			counter: func() float64 {
				return testutil.ToFloat64(
					RoundsCompletedTotal.WithLabelValues(
						"confirmed",
					),
				)
			},
		},
		{
			name: "oor sent submitted",
			msg: &OORTransferSentMsg{
				Status: "submitted",
			},
			counter: func() float64 {
				return testutil.ToFloat64(
					OORTransfersSentTotal.WithLabelValues(
						"submitted",
					),
				)
			},
		},
		{
			name: "oor received materialized",
			msg: &OORTransferReceivedMsg{
				Status: "materialized",
			},
			counter: func() float64 {
				return testutil.ToFloat64(
					OORTransfersReceivedTotal.
						WithLabelValues(
							"materialized",
						),
				)
			},
		},
		{
			name: "boarding submitted",
			msg: &BoardingEventMsg{
				Status: "submitted",
			},
			counter: func() float64 {
				return testutil.ToFloat64(
					BoardingEventsTotal.WithLabelValues(
						"submitted",
					),
				)
			},
		},
		{
			name: "background task error",
			msg: &BackgroundTaskErrorMsg{
				Task: "sweep_watcher",
			},
			counter: func() float64 {
				return testutil.ToFloat64(
					BackgroundTaskErrorsTotal.
						WithLabelValues(
							"sweep_watcher",
						),
				)
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: counters are package-global, so each
			// case must read its own before/after delta serially.
			a := NewMetricsActor(ActorConfig{})

			before := tc.counter()
			res := a.Receive(context.Background(), tc.msg)
			require.NoError(t, res.Err())

			require.Equal(t, before+1, tc.counter())
		})
	}
}

// TestMetricsActorPoolRoundRobin verifies that registering several
// metrics actors under the shared ActorKey and Telling through the sink
// resolved by NewSink fans events across the pool (the framework's
// default round-robin router) and that every event still lands on the
// shared, concurrency-safe Prometheus counter. This is the mechanism
// behind the metricsActorWorkers pool the daemon spawns: a stateless
// actor replicated N times, one sink, no producer-side change.
func TestMetricsActorPoolRoundRobin(t *testing.T) {
	t.Parallel()

	sys := actor.NewActorSystem()
	t.Cleanup(func() {
		_ = sys.Shutdown(context.Background())
	})

	const (
		workers = 4
		events  = 40
	)

	for i := 0; i < workers; i++ {
		actor.RegisterWithSystem(
			sys, fmt.Sprintf("%s-%d", ActorName, i), ActorKey,
			NewMetricsActor(
				ActorConfig{},
			),
		)
	}

	sink := NewSink(sys)

	// Use a unique label series so the assertion is exact and immune to
	// the package-global counters other tests increment concurrently.
	const status = "pooltest-roundrobin"
	counter := func() float64 {
		return testutil.ToFloat64(
			RoundsCompletedTotal.WithLabelValues(status),
		)
	}

	require.Zero(t, counter())

	for i := 0; i < events; i++ {
		err := sink.Tell(
			context.Background(), &RoundCompletedMsg{
				Status: status,
			},
		)
		require.NoError(t, err)
	}

	// Emission fans out across the worker pool and is processed
	// asynchronously, so wait for every event to land on the shared
	// counter rather than reading it immediately.
	require.Eventually(t, func() bool {
		return counter() == float64(events)
	}, 5*time.Second, 10*time.Millisecond)
}

// TestMetricsActorUnknownMessage verifies an unrecognized message type
// is tolerated (logged, not panicked) and yields an OK result.
func TestMetricsActorUnknownMessage(t *testing.T) {
	t.Parallel()

	a := NewMetricsActor(ActorConfig{})

	require.NotPanics(t, func() {
		res := a.Receive(context.Background(), unknownMsg{})
		require.NoError(t, res.Err())
	})
}

// unknownMsg is a Msg the actor's switch does not handle, used to
// exercise the default branch. It embeds actor.BaseMessage to satisfy
// the sealed actor.Message interface.
type unknownMsg struct {
	actor.BaseMessage
}

// MessageType implements actor.Message.
func (unknownMsg) MessageType() string { return "metrics.Unknown" }

func (unknownMsg) metricsMsgSealed() {}
