package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/unroll"
	"github.com/stretchr/testify/require"
)

// TestVirtualChannelBackingPublished verifies the lnd publish hook only
// unblocks after the backing spend has entered the unroll broadcast path.
func TestVirtualChannelBackingPublished(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phase unroll.Phase
		want  bool
	}{
		{
			name:  "materializing",
			phase: unroll.PhaseMaterializing,
		},
		{
			name:  "csv pending",
			phase: unroll.PhaseCSVPending,
		},
		{
			name:  "sweep broadcast",
			phase: unroll.PhaseSweepBroadcast,
		},
		{
			name:  "sweep confirmation",
			phase: unroll.PhaseSweepConfirmation,
			want:  true,
		},
		{
			name:  "completed",
			phase: unroll.PhaseCompleted,
			want:  true,
		},
		{
			name:  "failed",
			phase: unroll.PhaseFailed,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.want,
				virtualChannelBackingPublished(test.phase),
			)
		})
	}
}
