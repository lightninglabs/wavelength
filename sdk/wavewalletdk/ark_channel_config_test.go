//go:build swapruntime && wavewalletrpc

package wavewalletdk

import (
	"runtime"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedArkChannelConfig exercises the production startup merge on
// native hosts and JS/WASM. The advanced config remains caller-owned.
func TestEmbeddedArkChannelConfig(t *testing.T) {
	base := waved.DefaultConfig()
	cfg := Config{DaemonConfig: base}
	resolved, err := resolveDaemonConfig(cfg)
	require.NoError(t, err)
	require.False(t, resolved.Swap.SuppressArkChannelRuntime)
	require.False(t, resolved.Swap.ArkChannelsEnabled)

	cfg.ArkChannelsEnabled = true
	cfg.ServerTransport = TransportREST
	cfg.SwapServerTransport = TransportREST
	resolved, err = resolveDaemonConfig(cfg)
	require.NoError(t, err)
	require.False(t, resolved.Swap.SuppressArkChannelRuntime)
	require.True(t, resolved.Swap.ArkChannelsEnabled)
	require.False(t, base.Swap.ArkChannelsEnabled)

	base.Swap.ArkChannelsEnabled = true
	cfg.ArkChannelsEnabled = false
	resolved, err = resolveDaemonConfig(cfg)
	require.NoError(t, err)
	require.False(t, resolved.Swap.SuppressArkChannelRuntime)

	cfg.HostDrivenIngress = true
	_, err = resolveDaemonConfig(cfg)
	require.ErrorContains(t, err, "host_driven_ingress")
	cfg.HostDrivenIngress = false
	for _, wallet := range []string{
		waved.WalletTypeBtcwallet, waved.WalletTypeLnd,
	} {
		cfg.WalletType = wallet
		_, err = resolveDaemonConfig(cfg)
		require.ErrorContains(t, err, "requires lwwallet")
	}

	cfg.WalletType = waved.WalletTypeLwwallet
	for _, transports := range [][2]Transport{
		{
			TransportGRPC,
			TransportREST,
		},
		{
			TransportREST,
			TransportGRPC,
		},
		{
			TransportGRPC,
			TransportGRPC,
		},
	} {
		cfg.ServerTransport = transports[0]
		cfg.SwapServerTransport = transports[1]
		_, err = resolveDaemonConfig(cfg)
		if runtime.GOOS == "js" {
			require.ErrorContains(t, err, "REST")
		} else {
			require.NoError(t, err)
		}
	}
	if runtime.GOOS == "js" {
		cfg.ServerTransport = TransportREST
		cfg.SwapServerTransport = TransportREST
		cfg.ServerTLSCertPath = "operator.pem"
		_, err = resolveDaemonConfig(cfg)
		require.ErrorContains(t, err, "cannot pin")
		cfg.ServerTLSCertPath = ""
		cfg.SwapServerTLSCertPath = "swap.pem"
		_, err = resolveDaemonConfig(cfg)
		require.ErrorContains(t, err, "cannot pin")
	}
}
