//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBackgroundWindowBudgetRejectsOutsidePlatformCeiling checks that invalid
// budgets are rejected before any singleton access. Longer valid budgets reach
// the ordinary absent-client gate rather than the old twenty-second rejection.
func TestBackgroundWindowBudgetRejectsOutsidePlatformCeiling(t *testing.T) {
	for _, seconds := range []int64{-1, 0, 31} {
		_, err := RunReceiptWindow(nil, seconds)
		require.ErrorContains(t, err, "1..30 seconds")
		_, err = RunMaintenanceWindow("window", seconds)
		require.ErrorContains(t, err, "1..30 seconds")
	}
	for _, seconds := range []int64{1, 20, 28, 30} {
		_, err := RunReceiptWindow(nil, seconds)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "budget must be")
		_, err = RunMaintenanceWindow("window", seconds)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "budget must be")
	}
}
