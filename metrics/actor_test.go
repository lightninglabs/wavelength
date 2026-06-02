package metrics

import (
	"context"
	"testing"

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
			msg:  &RoundCompletedMsg{Status: "confirmed"},
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
			msg:  &OORTransferSentMsg{Status: "submitted"},
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
			msg:  &OORTransferReceivedMsg{Status: "materialized"},
			counter: func() float64 {
				return testutil.ToFloat64(
					OORTransfersReceivedTotal.
						WithLabelValues("materialized"),
				)
			},
		},
		{
			name: "boarding submitted",
			msg:  &BoardingEventMsg{Status: "submitted"},
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
			msg:  &BackgroundTaskErrorMsg{Task: "sweep_watcher"},
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
