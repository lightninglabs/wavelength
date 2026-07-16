package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReorgSafetyDepthResolution pins the inclusive policy boundary and its
// zero-value default.
func TestReorgSafetyDepthResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		configured   uint32
		wantHorizon  uint32
		wantFinality uint32
	}{
		{
			name:         "zero uses default",
			wantHorizon:  DefaultReorgSafetyDepth,
			wantFinality: DefaultReorgSafetyDepth + 1,
		},
		{
			name:         "custom",
			configured:   42,
			wantHorizon:  42,
			wantFinality: 43,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{ReorgSafetyDepth: tc.configured}
			require.Equal(t, tc.wantHorizon, cfg.reorgSafetyDepth())
			require.Equal(
				t, tc.wantFinality, cfg.chainFinalityDepth(),
			)
		})
	}
}

// TestConfigValidateRejectsUnsupportedReorgSafetyDepth verifies that policy
// cannot outlive the shortest current backend observation horizon.
func TestConfigValidateRejectsUnsupportedReorgSafetyDepth(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.ReorgSafetyDepth = MaxReorgSafetyDepth + 1

	err := cfg.Validate()
	require.ErrorContains(t, err, "reorgsafetydepth exceeds maximum")
}

// TestDefaultConfigHasReorgSafetyDepth locks in a non-zero policy default.
func TestDefaultConfigHasReorgSafetyDepth(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Equal(t, DefaultReorgSafetyDepth, cfg.ReorgSafetyDepth)
	require.Equal(t, DefaultReorgSafetyDepth+1, cfg.chainFinalityDepth())
}
