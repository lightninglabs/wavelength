//go:build itest

package main

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestStressClientNamesAreZeroPadded verifies generated stress names stay
// stable and sort naturally for larger client sets.
func TestStressClientNamesAreZeroPadded(t *testing.T) {
	require.Equal(t, []string{
		"client01", "client02", "client03",
	}, stressClientNames(3))

	names := stressClientNames(101)
	require.Equal(t, "client001", names[0])
	require.Equal(t, "client101", names[len(names)-1])
}

// TestStressBudgetHonorsDisabledRestarts verifies a disabled restart class
// does not keep the workload alive after payment and round budgets are spent.
func TestStressBudgetHonorsDisabledRestarts(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			maxPayments:      1,
			maxRounds:        1,
			maxRestarts:      10,
			clientRestarts:   false,
			operatorRestarts: false,
		},
		summary: stressSummary{
			PaymentsAttempted: 1,
			RoundsTriggered:   1,
		},
	}

	require.False(t, runner.hasBudget())
}

// TestRoundStateAtLeastUsesLifecycleOrder verifies client round waiters do not
// depend on protobuf enum numeric order.
func TestRoundStateAtLeastUsesLifecycleOrder(t *testing.T) {
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
	))
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_IDLE,
		daemonrpc.RoundState_ROUND_STATE_IDLE,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_IDLE,
		daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	))
	require.True(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED,
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
	))
	require.False(t, roundStateAtLeast(
		daemonrpc.RoundState_ROUND_STATE_FAILED,
		daemonrpc.RoundState_ROUND_STATE_IDLE,
	))
}

// TestStressBudgetIncludesClientCrashes verifies crash events share the
// restart budget and keep the workload eligible while crash budget remains.
func TestStressBudgetIncludesClientCrashes(t *testing.T) {
	runner := &stressRunner{
		cfg: stressConfig{
			maxRestarts:      1,
			clientCrashes:    true,
			clientRestarts:   false,
			operatorRestarts: false,
		},
	}

	require.True(t, runner.hasBudget())

	runner.summary.ClientCrashes = 1
	require.False(t, runner.hasBudget())
}

// TestStressFinalSummaryMetrics verifies derived latency, success-rate, and
// throughput fields are stable snapshots of the runner counters.
func TestStressFinalSummaryMetrics(t *testing.T) {
	started := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	completed := started.Add(4 * time.Second)
	runner := &stressRunner{
		cfg: stressConfig{
			seed:        42,
			concurrency: 3,
		},
		started: started,
		state: &harnessState{
			RunDir: "/tmp/arktest",
		},
		names: []string{"client01", "client02"},
		summary: stressSummary{
			PaymentsAttempted: 5,
			PaymentsSettled:   4,
			PaymentsFailed:    1,
			RoundsTriggered:   2,
			RoundsConfirmed:   1,
			RoundsFailed:      1,
		},
		paymentLatencies: []time.Duration{
			100 * time.Millisecond,
			200 * time.Millisecond,
			300 * time.Millisecond,
			1_000 * time.Millisecond,
		},
	}

	summary := runner.finalSummary(completed)

	require.Equal(t, int64(42), summary.Seed)
	require.Equal(t, int64(4_000), summary.DurationMS)
	require.Equal(t, 2, summary.Clients)
	require.Equal(t, 3, summary.Concurrency)
	require.Equal(t, 80.0, summary.PaymentSuccessPct)
	require.Equal(t, int64(400), summary.PaymentAvgMS)
	require.Equal(t, int64(200), summary.PaymentP50MS)
	require.Equal(t, int64(1_000), summary.PaymentP95MS)
	require.Equal(t, int64(1_000), summary.PaymentMaxMS)
	require.InDelta(t, 1.0, summary.PaymentThroughput, 0.001)
	require.Equal(t, 2, summary.RoundsTriggered)
	require.Equal(t, 1, summary.RoundsConfirmed)
	require.Equal(t, 1, summary.RoundsFailed)
}

// TestPercentileDurationUsesNearestRank verifies the percentile helper's
// nearest-rank behavior at the edges and middle of a sorted sample.
func TestPercentileDurationUsesNearestRank(t *testing.T) {
	sorted := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
	}

	require.Equal(t, 10*time.Millisecond, percentileDuration(sorted, 0))
	require.Equal(t, 20*time.Millisecond, percentileDuration(sorted, 50))
	require.Equal(t, 40*time.Millisecond, percentileDuration(sorted, 95))
	require.Equal(t, time.Duration(0), percentileDuration(nil, 95))
}
