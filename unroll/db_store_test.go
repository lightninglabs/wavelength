package unroll

import (
	"testing"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/stretchr/testify/require"
)

// TestTriggerStringRoundTrip verifies the durable trigger strings decode back
// into their enum values. This keeps DB rows and logs stable across restarts.
func TestTriggerStringRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		trigger StartTrigger
		want    string
	}{
		{
			TriggerManual,
			"manual",
		},
		{
			TriggerCriticalExpiry,
			"critical_expiry",
		},
		{
			TriggerRestart,
			"restart",
		},
		{
			TriggerFraudSpend,
			"fraud_spend",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()

			got := triggerToString(tc.trigger)
			require.Equal(t, tc.want, got)

			roundTrip, err := triggerFromString(got)
			require.NoError(t, err)
			require.Equal(t, tc.trigger, roundTrip)
		})
	}
}

// TestRegistryExitPolicyTreatsKindAndRefAsPair verifies policy identity
// preservation handles kind and ref atomically when a registry update refines
// an existing unroll DB row.
func TestRegistryExitPolicyTreatsKindAndRefAsPair(t *testing.T) {
	t.Parallel()

	existing := &db.UnrollJobRecord{
		ExitPolicyKind: "vhtlc_claim",
		ExitPolicyRef:  "recovery-1",
	}

	kind, ref := registryExitPolicy(RegistryRecord{}, existing)
	require.Equal(t, "vhtlc_claim", kind)
	require.Equal(t, "recovery-1", ref)

	kind, ref = registryExitPolicy(RegistryRecord{
		ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
	}, existing)
	require.Equal(t, StandardVTXOTimeoutExitPolicyKind, kind)
	require.Empty(t, ref)
}
