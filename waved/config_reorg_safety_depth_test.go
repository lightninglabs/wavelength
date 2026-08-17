package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateReorgSafetyDepthWithGate asserts that when the fail-closed
// batch-canonicality gate is enabled, a zero reorg-safety depth is rejected so
// the horizon is configured explicitly and deliberately matches the server,
// rather than silently resolving to the built-in default on the client while
// the server treats zero as immediate finality.
func TestConfigValidateReorgSafetyDepthWithGate(t *testing.T) {
	t.Parallel()

	base := func() *Config {
		cfg := DefaultConfig()
		cfg.Network = "regtest"
		cfg.Server.Host = "127.0.0.1:10010"
		cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"

		return cfg
	}

	t.Run("zero depth, gate off is valid", func(t *testing.T) {
		t.Parallel()

		cfg := base()
		cfg.ReorgSafetyDepth = 0
		cfg.BatchCanonicalityGate = false
		require.NoError(t, cfg.Validate())
	})

	t.Run("zero depth, gate on is rejected", func(t *testing.T) {
		t.Parallel()

		cfg := base()
		cfg.ReorgSafetyDepth = 0
		cfg.BatchCanonicalityGate = true

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(
			t, err.Error(),
			"reorgsafetydepth must be set explicitly",
		)
	})

	t.Run("nonzero depth, gate on is valid", func(t *testing.T) {
		t.Parallel()

		cfg := base()
		cfg.ReorgSafetyDepth = 30
		cfg.BatchCanonicalityGate = true
		require.NoError(t, cfg.Validate())
	})
}
