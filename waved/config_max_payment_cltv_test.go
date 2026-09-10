package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateMaxPaymentCLTV verifies malformed negative reserves fail
// at startup while zero continues to provide an explicit opt-out.
func TestConfigValidateMaxPaymentCLTV(t *testing.T) {
	t.Parallel()

	newConfig := func() *Config {
		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Server.Host = "127.0.0.1:10010"
		cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"

		return cfg
	}

	negative := newConfig()
	negative.MaxPaymentCLTV = -1
	require.ErrorContains(
		t, negative.Validate(),
		"maxpaymentcltv must be non-negative",
	)

	disabled := newConfig()
	disabled.MaxPaymentCLTV = 0
	require.NoError(t, disabled.Validate())

	enabled := newConfig()
	enabled.MaxPaymentCLTV = 300
	require.NoError(t, enabled.Validate())
}
