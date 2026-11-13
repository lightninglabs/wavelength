package swaps

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/stretchr/testify/require"
)

// TestReceiveClaimRejectsTimestampDeadline also protects already-funded legacy
// sessions: an old timestamp must not be mistaken for a future block height.
func TestReceiveClaimRejectsTimestampDeadline(t *testing.T) {
	t.Parallel()
	h := newClaimSessionHarness(t, false)
	h.session.vhtlcConfig.RefundLocktime = txscript.LockTimeThreshold
	err := h.session.ensureReceiveClaimStillPossible(t.Context())
	require.ErrorIs(t, err, errReceiveCooperativeDeadline)
	require.NoError(t, h.session.claimFundedVHTLC(t.Context()))
	require.Zero(t, h.daemon.sendCustomCalls)
	require.Zero(t, h.daemon.escalateCalls)
}

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

// TestReceiveClaimRecoveryDefersWithoutHeight keeps grace expiry from starting
// an exit whose claim window cannot be evaluated against chain state.
func TestReceiveClaimRecoveryDefersWithoutHeight(t *testing.T) {
	t.Parallel()

	h := newClaimSessionHarness(t, true)
	h.session.client.SetRecoveryPolicy(RecoveryPolicy{
		AutoEscalate:                  true,
		CooperativeFailureGracePeriod: 0,
		MinRecoveryMarginBlocks:       12,
	})

	// Even disabled grace must not bypass the absolute claim deadline.
	h.daemon.blockHeightErr = errors.New("block height unavailable")

	err := h.session.maybeEscalateReceiveClaimRecovery(
		t.Context(), errors.New("claim send failed"),
	)
	require.NoError(t, err)
	require.Zero(t, h.daemon.escalateCalls)
}

// TestReceiveClaimRecoveryRequiresViableWindow exercises the ancestry/CSV
// boundary before either grace expiry or deadline pressure can spend fees.
func TestReceiveClaimRecoveryRequiresViableWindow(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		height uint32
		grace  time.Duration
		want   int
	}{
		{
			"grace elapsed",
			100,
			0,
			1,
		},
		{
			"before pressure",
			203,
			time.Hour,
			0,
		},
		{
			"start deadline margin",
			204,
			time.Hour,
			1,
		},
		{
			"last viable block",
			215,
			time.Hour,
			1,
		},
		{
			"ancestry plus csv boundary",
			216,
			0,
			0,
		},
		{
			"refund cutoff",
			295,
			0,
			0,
		},
		{
			"refund mature",
			300,
			0,
			0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newClaimSessionHarness(t, true)
			h.daemon.blockHeight = test.height
			h.session.client.SetRecoveryPolicy(RecoveryPolicy{
				AutoEscalate:                  true,
				CooperativeFailureGracePeriod: test.grace,
			})
			err := h.session.maybeEscalateReceiveClaimRecovery(
				t.Context(), errors.New("cooperative claim "+
					"failed"),
			)
			require.NoError(t, err)
			require.Equal(t, test.want, h.daemon.escalateCalls)
			require.Zero(t, h.daemon.sendCustomCalls)
		})
	}
}
