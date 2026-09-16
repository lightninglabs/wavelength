package waveclicommands

import (
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestServiceFlags requires explicit consent and preserves a zero fee cap.
func TestServiceFlags(t *testing.T) {
	constructors := []struct {
		name       string
		newCommand func() *cobra.Command
	}{
		{
			"board",
			newBoardCmd,
		},
		{
			"refresh",
			newVTXOsRefreshCmd,
		},
		{
			"leave",
			newVTXOsLeaveCmd,
		},
	}
	for _, constructor := range constructors {
		t.Run(constructor.name, func(t *testing.T) {
			cmd := constructor.newCommand()
			service, err := serviceFromFlags(cmd)
			require.NoError(t, err)
			require.Nil(t, service)

			f := cmd.Flags()
			require.NoError(t, f.Set("service", "scheduled"))
			require.NoError(
				t,
				f.Set(
					"operation-id",
					strings.Repeat("01", 32),
				),
			)
			require.NoError(
				t, f.Set(
					"operation-expiry",
					"2030-01-01T00:00:00Z",
				),
			)
			require.NoError(t, f.Set("schedule-version", "1"))
			_, err = serviceFromFlags(cmd)
			require.ErrorContains(t, err, "service-fee-limit-sat")
			require.NoError(t, f.Set("service-fee-limit-sat", "0"))
			service, err = serviceFromFlags(cmd)
			require.NoError(t, err)
			require.Equal(
				t, roundpb.ServiceMode_SERVICE_SCHEDULED,
				service.Mode,
			)
			require.Zero(t, service.FeeLimitSat)
			require.False(t, service.AllowFallback)
			require.NoError(
				t, f.Set("allow-immediate-fallback", "true"),
			)
			service, err = serviceFromFlags(cmd)
			require.NoError(t, err)
			require.True(t, service.AllowFallback)
			if f.Lookup("no-join") != nil {
				require.NoError(t, f.Set("no-join", "true"))
				_, err = serviceFromFlags(cmd)
				require.ErrorContains(t, err, "no-join")
			}
		})
	}
}

// TestServiceFlagsRejectOrphanConsent prevents flags from becoming no-ops.
func TestServiceFlagsRejectOrphanConsent(t *testing.T) {
	cmd := newBoardCmd()
	require.NoError(t, cmd.Flags().Set("allow-immediate-fallback", "true"))
	_, err := serviceFromFlags(cmd)
	require.ErrorContains(t, err, "requires --service")
}
