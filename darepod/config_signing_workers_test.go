package darepod

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSigningWorkersConfig verifies automatic, serial, and invalid worker
// configuration values.
func TestSigningWorkersConfig(t *testing.T) {
	t.Parallel()

	t.Run("default selects automatic mode", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		require.Equal(t, DefaultSigningWorkers, cfg.SigningWorkers)
	})

	t.Run("explicit serial mode is valid", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Wallet.EsploraURL = "http://localhost:3002"
		cfg.SigningWorkers = 1
		require.NoError(t, cfg.Validate())
	})

	t.Run("negative worker count is rejected", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Wallet.EsploraURL = "http://localhost:3002"
		cfg.SigningWorkers = -1
		err := cfg.Validate()
		require.ErrorContains(
			t, err, "signingworkers must be non-negative",
		)
	})

	t.Run("excessive worker count is rejected", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Wallet.EsploraURL = "http://localhost:3002"
		cfg.SigningWorkers = MaxSigningWorkers + 1
		err := cfg.Validate()
		require.ErrorContains(
			t, err, "signingworkers exceeds maximum",
		)
	})
}

// TestSigningWorkerCount verifies backend-aware automatic selection while
// preserving an explicit operator override.
func TestSigningWorkerCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		walletType string
		configured int
		expected   int
	}{
		{
			name:       "lwwallet automatic",
			walletType: WalletTypeLwwallet,
			expected:   DefaultLwwalletSigningWorkers,
		},
		{
			name:       "btcwallet automatic",
			walletType: WalletTypeBtcwallet,
			expected:   1,
		},
		{
			name:       "remote lnd automatic",
			walletType: WalletTypeLnd,
			expected:   1,
		},
		{
			name:       "explicit override",
			walletType: WalletTypeLnd,
			configured: 8,
			expected:   8,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected, signingWorkerCount(
					test.walletType, test.configured,
				),
			)
		})
	}
}
