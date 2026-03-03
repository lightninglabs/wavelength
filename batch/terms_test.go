package batch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestValidateFundPsbtLockDuration verifies that the startup safety
// check rejects lock durations that are too short to cover the
// worst-case round lifetime.
func TestValidateFundPsbtLockDuration(t *testing.T) {
	t.Parallel()

	t.Run("zero duration rejected", func(t *testing.T) {
		t.Parallel()

		terms := &Terms{
			RegistrationTimeout:        30 * time.Second,
			SignatureCollectionTimeout: 30 * time.Second,
		}
		err := terms.ValidateFundPsbtLockDuration()
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be set")
	})

	t.Run("too short rejected", func(t *testing.T) {
		t.Parallel()

		terms := &Terms{
			RegistrationTimeout:        30 * time.Second,
			SignatureCollectionTimeout: 30 * time.Second,
			// min = 30 + 3*30 = 120s = 2min
			FundPsbtLockDuration: 2 * time.Minute,
		}
		err := terms.ValidateFundPsbtLockDuration()
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be greater")
	})

	t.Run("sufficient duration accepted", func(t *testing.T) {
		t.Parallel()

		terms := &Terms{
			RegistrationTimeout:        30 * time.Second,
			SignatureCollectionTimeout: 30 * time.Second,
			FundPsbtLockDuration:       DefaultFundPsbtLockDuration,
		}
		err := terms.ValidateFundPsbtLockDuration()
		require.NoError(t, err)
	})

	t.Run("exactly at minimum rejected", func(t *testing.T) {
		t.Parallel()

		terms := &Terms{
			RegistrationTimeout:        1 * time.Minute,
			SignatureCollectionTimeout: 1 * time.Minute,
			// min = 1 + 3*1 = 4min, so 4min is NOT enough
			// (strictly greater required).
			FundPsbtLockDuration: 4 * time.Minute,
		}
		err := terms.ValidateFundPsbtLockDuration()
		require.Error(t, err)

		// 4min + 1s is enough.
		terms.FundPsbtLockDuration = 4*time.Minute + time.Second
		err = terms.ValidateFundPsbtLockDuration()
		require.NoError(t, err)
	})
}
