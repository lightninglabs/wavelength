package btcwbackend

import (
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/stretchr/testify/require"
)

// TestNeutrinoHeadersImportConfig verifies that btcwallet fast-sync header
// sources are validated and mapped into neutrino's import configuration.
func TestNeutrinoHeadersImportConfig(t *testing.T) {
	t.Parallel()

	cfg, err := neutrinoHeadersImportConfig("", "")
	require.NoError(t, err)
	require.Nil(t, cfg)

	cfg, err = neutrinoHeadersImportConfig("blocks.bin", "filters.bin")
	require.NoError(t, err)
	require.Equal(t, "blocks.bin", cfg.BlockHeadersSource)
	require.Equal(t, "filters.bin", cfg.FilterHeadersSource)
	require.Equal(t, blockchain.BFFastAdd, cfg.ValidationFlags)
}

// TestNewNeutrinoServiceRequiresChainParams checks that invalid chain params
// fail before the service touches filesystem or database resources.
func TestNewNeutrinoServiceRequiresChainParams(t *testing.T) {
	t.Parallel()

	_, err := NewNeutrinoService(
		t.TempDir(), nil, nil, nil, false, "", "", nil,
	)
	require.ErrorContains(t, err, "chain params are required")
}

// TestNeutrinoHeadersImportConfigRequiresBothSources checks that a partial
// import configuration fails before neutrino opens the P2P backend.
func TestNeutrinoHeadersImportConfigRequiresBothSources(t *testing.T) {
	t.Parallel()

	_, err := neutrinoHeadersImportConfig("blocks.bin", "")
	require.ErrorContains(t, err, "must be specified together")

	_, err = neutrinoHeadersImportConfig("", "filters.bin")
	require.ErrorContains(t, err, "must be specified together")
}
