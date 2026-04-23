package unroll

import (
	"testing"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/stretchr/testify/require"
)

// TestPhaseDBRoundTrip pins the Phase↔UnilateralExitJobStatus mapping so
// schema drift or table-entry reshuffles fail loudly rather than silently
// collapsing distinct phases onto a single DB status (which previously
// erased the "sweep built but not yet broadcast" vs "sweep broadcast
// awaiting confirmation" distinction).
func TestPhaseDBRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		phase Phase
		want  db.UnilateralExitJobStatus
	}{
		{PhasePending, db.UnilateralExitJobStatusPending},
		{PhaseMaterializing, db.UnilateralExitJobStatusMaterializing},
		{PhaseCSVPending, db.UnilateralExitJobStatusCSVPending},
		{
			PhaseSweepBroadcast,
			db.UnilateralExitJobStatusSweepBroadcasting,
		},
		{
			PhaseSweepConfirmation,
			db.UnilateralExitJobStatusSweeping,
		},
		{PhaseCompleted, db.UnilateralExitJobStatusCompleted},
		{PhaseFailed, db.UnilateralExitJobStatusFailed},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()

			gotStatus := statusForPhase(tc.phase)
			require.Equal(t, tc.want, gotStatus,
				"statusForPhase(%q)", tc.phase)

			gotPhase := phaseFromDB(gotStatus)
			require.Equal(t, tc.phase, gotPhase,
				"round-trip phaseFromDB(statusForPhase(%q))",
				tc.phase)
		})
	}
}

// TestTriggerDBRoundTrip pins the StartTrigger↔UnilateralExitJobTrigger
// mapping so FraudSpend rows round-trip through a dedicated constant
// rather than silently decoding as TriggerManual.
func TestTriggerDBRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		trigger StartTrigger
		want    db.UnilateralExitJobTrigger
	}{
		{TriggerManual, db.UnilateralExitJobTriggerManual},
		{
			TriggerCriticalExpiry,
			db.UnilateralExitJobTriggerCriticalExpiry,
		},
		{TriggerRestart, db.UnilateralExitJobTriggerRestart},
		{
			TriggerFraudSpend,
			db.UnilateralExitJobTriggerFraudSpend,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(triggerName(tc.trigger), func(t *testing.T) {
			t.Parallel()

			gotDB := triggerToDB(tc.trigger)
			require.Equal(t, tc.want, gotDB,
				"triggerToDB(%v)", tc.trigger)

			gotTrigger := triggerFromDB(gotDB)
			require.Equal(t, tc.trigger, gotTrigger,
				"round-trip triggerFromDB(triggerToDB(%v))",
				tc.trigger)
		})
	}
}

// triggerName returns a stable subtest name for a StartTrigger.
func triggerName(t StartTrigger) string {
	switch t {
	case TriggerManual:
		return "manual"

	case TriggerCriticalExpiry:
		return "critical_expiry"

	case TriggerRestart:
		return "restart"

	case TriggerFraudSpend:
		return "fraud_spend"

	default:
		return "unknown"
	}
}
