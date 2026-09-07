package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnrollSweepFeeRateFallbackRestrictsFixedRateToLocalNetworks verifies
// public networks defer sweep construction when fee estimation fails, while
// regtest and simnet keep an explicit fallback for controlled block production.
func TestUnrollSweepFeeRateFallbackRestrictsFixedRateToLocalNetworks(
	t *testing.T) {

	require.Zero(t, (&Server{}).unrollSweepFeeRateFallback())
	require.Zero(
		t, (&Server{
			cfg: &Config{Network: "mainnet"},
		}).unrollSweepFeeRateFallback(),
	)
	require.Equal(
		t, int64(2), (&Server{
			cfg: &Config{Network: "regtest"},
		}).unrollSweepFeeRateFallback(),
	)
	require.Equal(
		t, int64(2), (&Server{
			cfg: &Config{Network: "simnet"},
		}).unrollSweepFeeRateFallback(),
	)
}
