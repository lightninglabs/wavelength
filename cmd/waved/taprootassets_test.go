package main

import (
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

// TestConfigureTaprootAssetsIsOptIn proves the standalone runtime only installs
// a tapd lifecycle hook after explicit configuration.
func TestConfigureTaprootAssetsIsOptIn(t *testing.T) {
	t.Parallel()

	disabled := waved.DefaultConfig()
	configureTaprootAssets(disabled)
	require.Empty(t, disabled.RPCServiceRegistrars)

	enabled := waved.DefaultConfig()
	enabled.Network = "regtest"
	enabled.TaprootAssets.Enabled = true
	configureTaprootAssets(enabled)
	require.Len(t, enabled.RPCServiceRegistrars, 1)
}

// TestTaprootAssetFlagsExposePoCConfiguration keeps the runnable adapter
// discoverable without enabling it implicitly.
func TestTaprootAssetFlagsExposePoCConfiguration(t *testing.T) {
	t.Parallel()

	command := newRootCmd()
	for _, name := range []string{
		"taprootassets.enabled",
		"taprootassets.host",
		"taprootassets.tlscertpath",
		"taprootassets.macaroonpath",
		"taprootassets.insecure",
		"taprootassets.rpctimeout",
		"taprootassets.preparationdir",
	} {
		require.NotNil(t, command.Flags().Lookup(name), name)
	}
	require.Equal(
		t, "false",
		command.Flags().Lookup("taprootassets.enabled").DefValue,
	)
}
