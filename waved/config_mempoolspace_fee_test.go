package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// lndBaseConfig returns a minimal valid lnd-backed config for fee-provider
// validation tests.
func lndBaseConfig() *Config {
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.Type = WalletTypeLnd
	cfg.Lnd.Host = "127.0.0.1:10009"

	return cfg
}

// TestMempoolSpaceFeeAccessorsAreNilSafe asserts the accessor helpers tolerate
// a partially-populated or absent fee-estimation config without panicking.
func TestMempoolSpaceFeeAccessorsAreNilSafe(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	require.False(t, cfg.MempoolSpaceFeeEnabled())
	require.Empty(t, cfg.MempoolSpaceFeeURL())

	cfg.FeeEstimation = &FeeEstimationConfig{}
	require.False(t, cfg.MempoolSpaceFeeEnabled())
	require.Empty(t, cfg.MempoolSpaceFeeURL())

	cfg.FeeEstimation.MempoolSpace = &MempoolSpaceFeeConfig{
		Enabled: true,
		URL:     "https://mempool.space/api/v1/fees/recommended",
	}
	require.True(t, cfg.MempoolSpaceFeeEnabled())
	require.Equal(
		t, "https://mempool.space/api/v1/fees/recommended",
		cfg.MempoolSpaceFeeURL(),
	)
}

// TestDefaultConfigDisablesMempoolSpaceFee locks in that the provider is off by
// default so an operator building from defaults never silently queries an
// external endpoint.
func TestDefaultConfigDisablesMempoolSpaceFee(t *testing.T) {
	t.Parallel()

	require.False(t, DefaultConfig().MempoolSpaceFeeEnabled())
}

// TestConfigValidateAcceptsMempoolSpaceFeeOnLnd asserts the provider validates
// on the lnd wallet backend.
func TestConfigValidateAcceptsMempoolSpaceFeeOnLnd(t *testing.T) {
	t.Parallel()

	cfg := lndBaseConfig()
	cfg.FeeEstimation.MempoolSpace.Enabled = true

	require.NoError(t, cfg.Validate())
}

// TestConfigValidateRejectsMempoolSpaceFeeOffLnd asserts enabling the provider
// on a non-lnd backend fails validation rather than silently doing nothing.
func TestConfigValidateRejectsMempoolSpaceFeeOffLnd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		walletType string
		setup      func(*Config)
	}{
		{
			name:       "lwwallet",
			walletType: WalletTypeLwwallet,
			setup: func(c *Config) {
				c.Wallet.EsploraURL = "http://127.0.0.1:3000"
			},
		},
		{
			name:       "btcwallet",
			walletType: WalletTypeBtcwallet,
			setup: func(c *Config) {
				c.Wallet.FeeURL = "http://127.0.0.1:3000/fees"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Server.Host = "127.0.0.1:10010"
			cfg.Wallet.Type = tc.walletType
			tc.setup(cfg)
			cfg.FeeEstimation.MempoolSpace.Enabled = true

			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(
				t, err.Error(),
				"feeestimation.mempoolspace is only supported",
			)
		})
	}
}
