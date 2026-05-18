package unroll

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
