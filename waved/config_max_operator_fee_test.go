package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateRejectsZeroMaxOperatorFee asserts that a
// non-positive MaxOperatorFeeSat fails validation. This closes the
// lazy-integrator fail-open where an unset cap would silently
// accept any server-quoted operator fee under the #270 seal-time
// handshake.
func TestConfigValidateRejectsZeroMaxOperatorFee(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value int64
	}{
		{
			name:  "zero",
			value: 0,
		},
		{
			name:  "negative",
			value: -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Server.Host = "127.0.0.1:10010"
			cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
			cfg.MaxOperatorFeeSat = tc.value

			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(),
				"maxoperatorfeesat")
		})
	}
}

// TestConfigValidateAcceptsPositiveMaxOperatorFee asserts a
// positive cap is accepted.
func TestConfigValidateAcceptsPositiveMaxOperatorFee(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.MaxOperatorFeeSat = 500_000

	require.NoError(t, cfg.Validate())
}

// TestDefaultConfigHasPositiveMaxOperatorFee locks in the default:
// DefaultConfig must produce a cap >0 so a user who builds from
// defaults never runs with fee rejection fail-closed by accident.
func TestDefaultConfigHasPositiveMaxOperatorFee(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Positive(t, cfg.MaxOperatorFeeSat)
	require.Equal(t, DefaultMaxOperatorFeeSat, cfg.MaxOperatorFeeSat)
}

// TestDefaultConfigDisablesAutomaticRefreshBudgets verifies the optional
// maintenance-specific caps preserve existing behavior unless configured.
func TestDefaultConfigDisablesAutomaticRefreshBudgets(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Zero(t, cfg.MaxAutoRefreshFeeSat)
	require.Zero(t, cfg.MaxAutoRefreshFeeRatePPM)
}

// TestConfigValidateAutomaticRefreshBudgets checks both public policy bounds.
func TestConfigValidateAutomaticRefreshBudgets(t *testing.T) {
	t.Parallel()

	newConfig := func() *Config {
		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Server.Host = "127.0.0.1:10010"
		cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"

		return cfg
	}

	negativeAbsolute := newConfig()
	negativeAbsolute.MaxAutoRefreshFeeSat = -1
	err := negativeAbsolute.Validate()
	require.ErrorContains(t, err, "maxautorefreshfeesat")

	invalidRate := newConfig()
	invalidRate.MaxAutoRefreshFeeRatePPM = 1_000_001
	err = invalidRate.Validate()
	require.ErrorContains(t, err, "maxautorefreshfeerateppm")

	valid := newConfig()
	valid.MaxAutoRefreshFeeSat = 10_000
	valid.MaxAutoRefreshFeeRatePPM = 25_000
	require.NoError(t, valid.Validate())
}
