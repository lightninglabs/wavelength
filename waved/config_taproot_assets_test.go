package waved

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTaprootAssetsConfigDisabledByDefault keeps ordinary Bitcoin-only daemon
// startup independent from tapd.
func TestTaprootAssetsConfigDisabledByDefault(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.NotNil(t, cfg.TaprootAssets)
	require.False(t, cfg.TaprootAssets.Enabled)
	require.Equal(t, DefaultTapdHost, cfg.TaprootAssets.Host)
}

// TestTaprootAssetsConfigRestrictsInsecureTransport prevents the PoC switch
// from disabling tapd authentication on public networks.
func TestTaprootAssetsConfigRestrictsInsecureTransport(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.AllowMainnet = true
	cfg.TaprootAssets.Enabled = true
	cfg.TaprootAssets.Insecure = true
	require.ErrorContains(t, cfg.Validate(), "restricted to regtest")

	cfg = DefaultConfig()
	cfg.Network = "regtest"
	cfg.Wallet.Type = "lnd"
	cfg.TaprootAssets.Enabled = true
	cfg.TaprootAssets.Insecure = true
	require.NoError(t, cfg.Validate())
}

// TestTaprootAssetsConfigExpandsPaths ensures tapd credentials and the journal
// follow the same home-directory normalization as other daemon secrets.
func TestTaprootAssetsConfigExpandsPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Wallet.Type = "lnd"
	cfg.TaprootAssets.Enabled = true
	cfg.TaprootAssets.TLSCertPath = "~/.tapd/tls.cert"
	cfg.TaprootAssets.MacaroonPath = "~/.tapd/admin.macaroon"
	cfg.TaprootAssets.PreparationDir = "~/.waved/taproot-assets-oor"
	require.NoError(t, cfg.Validate())
	require.Equal(
		t, filepath.Join(home, ".tapd", "tls.cert"),
		cfg.TaprootAssets.TLSCertPath,
	)
	require.Equal(
		t, filepath.Join(home, ".tapd", "admin.macaroon"),
		cfg.TaprootAssets.MacaroonPath,
	)
	require.Equal(
		t, filepath.Join(home, ".waved", "taproot-assets-oor"),
		cfg.TaprootAssets.PreparationDir,
	)
}
