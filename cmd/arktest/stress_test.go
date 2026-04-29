//go:build itest

package main

import (
	"testing"

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
