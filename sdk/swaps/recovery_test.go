package swaps

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDecideRecoveryEscalationAppliesPolicy verifies the SDK does not turn a
// cooperative retry failure into on-chain unroll until the configured policy
// allows it.
func TestDecideRecoveryEscalationAppliesPolicy(t *testing.T) {
	t.Parallel()

	firstFailure := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := firstFailure.Add(30 * time.Minute)
	policy := RecoveryPolicy{
		AutoEscalate:                  true,
		CooperativeFailureGracePeriod: time.Hour,
		MinRecoveryMarginBlocks:       12,
	}

	decision := decideRecoveryEscalation(policy, firstFailure, now, 100, 0)
	require.False(t, decision.Escalate)
	require.Equal(t, "within_grace_period", decision.Trigger)
	require.Equal(t, firstFailure.Add(time.Hour), decision.NextRetryAt)

	decision = decideRecoveryEscalation(
		policy, firstFailure, firstFailure.Add(time.Hour), 100, 0,
	)
	require.True(t, decision.Escalate)
	require.Equal(t, "grace_elapsed", decision.Trigger)

	decision = decideRecoveryEscalation(
		policy, firstFailure, now, 188, 200,
	)
	require.True(t, decision.Escalate)
	require.Equal(t, "deadline_margin", decision.Trigger)
	require.EqualValues(t, 12, decision.RemainingBlocks)
}

// TestDecideRecoveryEscalationRespectsDisabledAutoEscalate verifies manual
// recovery mode preserves the operator's explicit AutoEscalate=false setting.
func TestDecideRecoveryEscalationRespectsDisabledAutoEscalate(t *testing.T) {
	t.Parallel()

	firstFailure := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	policy := RecoveryPolicy{
		AutoEscalate:                  false,
		CooperativeFailureGracePeriod: time.Hour,
		MinRecoveryMarginBlocks:       12,
	}

	decision := decideRecoveryEscalation(
		policy, firstFailure, firstFailure.Add(24*time.Hour), 199, 200,
	)
	require.False(t, decision.Escalate)
	require.Equal(t, "auto_escalate_disabled", decision.Trigger)
}
