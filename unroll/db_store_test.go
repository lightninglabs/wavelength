package unroll

import (
	"testing"

	"github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// TestPhaseDBRoundTrip pins the Phase<->UnilateralExitJobStatus mapping so
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
		{
			PhasePending,
			db.UnilateralExitJobStatusPending,
		},
		{
			PhaseMaterializing,
			db.UnilateralExitJobStatusMaterializing,
		},
		{
			PhaseCSVPending,
			db.UnilateralExitJobStatusCSVPending,
		},
		{
			PhaseSweepBroadcast,
			db.UnilateralExitJobStatusSweepBroadcasting,
		},
		{
			PhaseSweepConfirmation,
			db.UnilateralExitJobStatusSweeping,
		},
		{
			PhaseCompleted,
			db.UnilateralExitJobStatusCompleted,
		},
		{
			PhaseFailed,
			db.UnilateralExitJobStatusFailed,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()

			gotStatus := statusForPhase(tc.phase)
			require.Equal(
				t, tc.want, gotStatus, "statusForPhase(%q)",
				tc.phase,
			)

			gotPhase := phaseFromDB(gotStatus)
			require.Equal(
				t, tc.phase, gotPhase, "round-trip "+
					"phaseFromDB(statusForPhase(%q))",
				tc.phase,
			)
		})
	}
}

// TestRecoverableFailureDBRoundTrip pins the mapping that distinguishes a
// recoverable (no-footprint) failure from a footprint-bearing one. A
// recoverable failure persists as the dedicated FailedRecoverable status and
// decodes back to RecoverableFailure=true, while a plain failure stays at the
// Failed status. Boot-time reconciliation relies on this distinction to
// decide whether to roll a VTXO back to live (wavelength#602).
func TestRecoverableFailureDBRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("recoverable", func(t *testing.T) {
		t.Parallel()

		rec := RegistryRecord{
			Phase:              PhaseFailed,
			RecoverableFailure: true,
		}
		status := statusForRecord(rec)
		require.Equal(
			t, db.UnilateralExitJobStatusFailedRecoverable, status,
		)
		require.True(t, status.IsTerminal())

		got := recordFromDB(db.UnilateralExitJobRecord{Status: status})
		require.Equal(t, PhaseFailed, got.Phase)
		require.True(t, got.RecoverableFailure)
	})

	t.Run("footprint", func(t *testing.T) {
		t.Parallel()

		rec := RegistryRecord{
			Phase:              PhaseFailed,
			RecoverableFailure: false,
		}
		status := statusForRecord(rec)
		require.Equal(t, db.UnilateralExitJobStatusFailed, status)

		got := recordFromDB(db.UnilateralExitJobRecord{Status: status})
		require.Equal(t, PhaseFailed, got.Phase)
		require.False(t, got.RecoverableFailure)
	})
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
		{
			TriggerManual,
			db.UnilateralExitJobTriggerManual,
		},
		{
			TriggerCriticalExpiry,
			db.UnilateralExitJobTriggerCriticalExpiry,
		},
		{
			TriggerRestart,
			db.UnilateralExitJobTriggerRestart,
		},
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
			require.Equal(
				t, tc.want, gotDB, "triggerToDB(%v)",
				tc.trigger,
			)

			gotTrigger := triggerFromDB(gotDB)
			require.Equal(
				t, tc.trigger, gotTrigger, "round-trip "+
					"triggerFromDB(triggerToDB(%v))",
				tc.trigger,
			)
		})
	}
}

// TestRegistryExitPolicyTreatsKindAndRefAsPair verifies policy identity
// preservation handles kind and ref atomically when a registry update refines
// an existing unilateral-exit DB row.
func TestRegistryExitPolicyTreatsKindAndRefAsPair(t *testing.T) {
	t.Parallel()

	existing := &db.UnilateralExitJobRecord{
		ExitPolicyKind: "vhtlc_claim",
		ExitPolicyRef:  "recovery-1",
	}

	kind, ref := registryExitPolicy(RegistryRecord{}, existing)
	require.Equal(t, ExitPolicyKind("vhtlc_claim"), kind)
	require.Equal(t, "recovery-1", ref)

	kind, ref = registryExitPolicy(RegistryRecord{
		ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
	}, existing)
	require.Equal(t, StandardVTXOTimeoutExitPolicyKind, kind)
	require.Empty(t, ref)
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
