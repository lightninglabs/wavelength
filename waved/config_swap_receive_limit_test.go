package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultMaxActiveReceiveSwaps pins the default accumulated worker bound.
func TestDefaultMaxActiveReceiveSwaps(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Equal(
		t, DefaultMaxActiveReceiveSwaps, cfg.Swap.MaxActiveReceiveSwaps,
	)
}

// TestMaxActiveReceiveSwapsZeroSelectsDefault verifies zero-value configs keep
// the safe default rather than disabling worker admission or the bound.
func TestMaxActiveReceiveSwapsZeroSelectsDefault(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.AllowMainnet = true
	cfg.Swap.MaxActiveReceiveSwaps = 0
	require.NoError(t, cfg.Validate())
	require.Equal(
		t, DefaultMaxActiveReceiveSwaps, cfg.Swap.MaxActiveReceiveSwaps,
	)
}

// TestMaxActiveReceiveSwapsRejectsNegative verifies invalid limits fail during
// configuration validation.
func TestMaxActiveReceiveSwapsRejectsNegative(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.AllowMainnet = true
	cfg.Swap.MaxActiveReceiveSwaps = -1
	err := cfg.Validate()
	require.ErrorContains(t, err, "must be non-negative")
}
