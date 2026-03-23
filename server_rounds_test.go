package darepo

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestRoundsTermsFromConfigIncludesConnectorDustAmount verifies the
// operator's round terms carry the configured connector dust amount.
func TestRoundsTermsFromConfigIncludesConnectorDustAmount(t *testing.T) {
	t.Parallel()

	cfg := DefaultRoundsConfig()
	cfg.ConnectorDustAmount = 777

	terms := roundsTermsFromConfig(cfg)

	require.Equal(
		t, btcutil.Amount(cfg.ConnectorDustAmount),
		terms.ConnectorDustAmount,
	)
}
