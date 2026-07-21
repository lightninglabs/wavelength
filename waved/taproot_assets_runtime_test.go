package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigureTaprootAssetsDisabled proves production registration remains
// explicitly opt-in and nil-safe for embedded consumers.
func TestConfigureTaprootAssetsDisabled(t *testing.T) {
	t.Parallel()

	ConfigureTaprootAssets(nil)

	cfg := DefaultConfig()
	ConfigureTaprootAssets(cfg)
	require.Empty(t, cfg.RPCServiceRegistrars)

	cfg.TaprootAssets = nil
	ConfigureTaprootAssets(cfg)
	require.Empty(t, cfg.RPCServiceRegistrars)
}

// TestConfigureTaprootAssetsRegistersOnce proves repeated setup paths install
// exactly one lazy production registrar.
func TestConfigureTaprootAssetsRegistersOnce(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.TaprootAssets.Enabled = true

	ConfigureTaprootAssets(cfg)
	require.Len(t, cfg.RPCServiceRegistrars, 1)
	require.True(t, cfg.taprootAssetsRuntimeConfigured)
	require.Nil(t, cfg.TaprootAssetOORPreparer)
	require.Nil(t, cfg.TaprootAssetOnboarder)

	ConfigureTaprootAssets(cfg)
	require.Len(t, cfg.RPCServiceRegistrars, 1)
}

// TestConfigureTaprootAssetsPreservesInjectedPreparer proves an embedding
// application can retain ownership of its explicitly supplied integration.
func TestConfigureTaprootAssetsPreservesInjectedPreparer(t *testing.T) {
	t.Parallel()

	preparer := &testTaprootAssetOORPreparer{}
	cfg := DefaultConfig()
	cfg.TaprootAssets.Enabled = true
	cfg.TaprootAssetOORPreparer = preparer

	ConfigureTaprootAssets(cfg)
	require.Empty(t, cfg.RPCServiceRegistrars)
	require.False(t, cfg.taprootAssetsRuntimeConfigured)
	require.Same(t, preparer, cfg.TaprootAssetOORPreparer)
}
