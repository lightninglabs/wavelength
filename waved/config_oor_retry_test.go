package waved

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateRejectsNegativeMaxTransientSubmitRetry asserts a negative
// transient submit-reject retry window is a misconfiguration and is rejected.
func TestConfigValidateRejectsNegativeMaxTransientSubmitRetry(t *testing.T) {
	t.Parallel()

	cfg := validOORLimitsTestConfig()
	cfg.OOR.MaxTransientSubmitRetry = -time.Second

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "oor.maxsubmitretry")
}

// TestConfigValidateDefaultsMaxTransientSubmitRetry asserts a zero (unset)
// transient submit-reject retry window is defaulted rather than left disabled.
func TestConfigValidateDefaultsMaxTransientSubmitRetry(t *testing.T) {
	t.Parallel()

	cfg := validOORLimitsTestConfig()
	cfg.OOR.MaxTransientSubmitRetry = 0

	require.NoError(t, cfg.Validate())
	require.Equal(
		t, defaultMaxTransientSubmitRetry,
		cfg.OOR.MaxTransientSubmitRetry,
	)
}

// TestConfigValidateAcceptsPositiveMaxTransientSubmitRetry asserts an explicit
// positive window is preserved through validation.
func TestConfigValidateAcceptsPositiveMaxTransientSubmitRetry(t *testing.T) {
	t.Parallel()

	cfg := validOORLimitsTestConfig()
	cfg.OOR.MaxTransientSubmitRetry = 90 * time.Minute

	require.NoError(t, cfg.Validate())
	require.Equal(
		t, 90*time.Minute, cfg.OOR.MaxTransientSubmitRetry,
	)
}
