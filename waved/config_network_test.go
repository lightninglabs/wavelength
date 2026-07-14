package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// TestConfigValidateAcceptsSupportedNetworks verifies the daemon accepts every
// public network string it advertises.
func TestConfigValidateAcceptsSupportedNetworks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		allowMainnet bool
	}{
		{
			name:         "mainnet",
			allowMainnet: true,
		},
		{
			name: "testnet",
		},
		{
			name: "testnet4",
		},
		{
			name: "regtest",
		},
		{
			name: "simnet",
		},
		{
			name: "signet",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = tc.name
			cfg.AllowMainnet = tc.allowMainnet
			cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"

			require.NoError(t, cfg.Validate())
			require.NotEmpty(t, cfg.ArkServerAddress())
			require.NotEmpty(t, cfg.SwapServerAddress())
			require.Empty(t, cfg.Server.Host)
			require.Empty(t, cfg.Swap.ServerAddress)
		})
	}
}

// TestConfigValidateAllowsTestnet4InsecureRPC verifies testnet4 remains a test
// network for daemon-local RPC security validation.
func TestConfigValidateAllowsTestnet4InsecureRPC(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "testnet4"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true

	require.NoError(t, cfg.Validate())
}

// TestConfigEndpointDefaults verifies the config resolves each empty endpoint
// from one network+transport table without mutating the explicit config fields.
func TestConfigEndpointDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      string
		transport    string
		wantArkHost  string
		wantSwapHost string
	}{
		{
			name:         "mainnet grpc",
			network:      "mainnet",
			transport:    RPCTransportGRPC,
			wantArkHost:  DefaultServerHost,
			wantSwapHost: defaultSwapServerHost,
		},
		{
			name:         "testnet3 grpc",
			network:      "testnet",
			transport:    RPCTransportGRPC,
			wantArkHost:  defaultTestnet3ServerGRPCHost,
			wantSwapHost: defaultTestnet3SwapServerGRPCHost,
		},
		{
			name:         "testnet3 rest",
			network:      "testnet",
			transport:    RPCTransportREST,
			wantArkHost:  defaultTestnet3ServerRESTHost,
			wantSwapHost: defaultTestnet3SwapServerRESTHost,
		},
		{
			name:         "testnet4 grpc",
			network:      "testnet4",
			transport:    RPCTransportGRPC,
			wantArkHost:  defaultTestnet4ServerGRPCHost,
			wantSwapHost: defaultTestnet4SwapServerGRPCHost,
		},
		{
			name:         "testnet4 rest",
			network:      "testnet4",
			transport:    RPCTransportREST,
			wantArkHost:  defaultTestnet4ServerRESTHost,
			wantSwapHost: defaultTestnet4SwapServerRESTHost,
		},
		{
			name:         "signet grpc",
			network:      "signet",
			transport:    RPCTransportGRPC,
			wantArkHost:  defaultSignetServerGRPCHost,
			wantSwapHost: defaultSignetSwapServerGRPCHost,
		},
		{
			name:         "signet rest",
			network:      "signet",
			transport:    RPCTransportREST,
			wantArkHost:  defaultSignetServerRESTHost,
			wantSwapHost: defaultSignetSwapServerRESTHost,
		},
		{
			name:         "regtest rest",
			network:      "regtest",
			transport:    RPCTransportREST,
			wantArkHost:  DefaultServerHost,
			wantSwapHost: defaultSwapServerHost,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = tc.network
			cfg.Server.Transport = tc.transport
			cfg.Swap.ServerTransport = tc.transport

			require.Empty(t, cfg.Server.Host)
			require.Empty(t, cfg.Swap.ServerAddress)
			require.Equal(t, tc.wantArkHost, cfg.ArkServerAddress())
			require.Equal(
				t, tc.wantSwapHost, cfg.SwapServerAddress(),
			)
		})
	}
}

// TestConfigEndpointDefaultsPreserveOverrides verifies an explicit address
// always wins, including a caller that deliberately points signet at localhost.
func TestConfigEndpointDefaultsPreserveOverrides(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "signet"
	cfg.Server.Host = "localhost:11010"
	cfg.Swap.ServerAddress = "localhost:11030"

	require.Equal(t, "localhost:11010", cfg.ArkServerAddress())
	require.Equal(t, "localhost:11030", cfg.SwapServerAddress())
}

// TestNetworkToChainParams verifies network strings map to their exact btcd
// chain parameters.
func TestNetworkToChainParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		network string
		want    *chaincfg.Params
		wantErr bool
	}{
		{
			network: "mainnet",
			want:    &chaincfg.MainNetParams,
		},
		{
			network: "testnet",
			want:    &chaincfg.TestNet3Params,
		},
		{
			network: "testnet4",
			want:    &chaincfg.TestNet4Params,
		},
		{
			network: "regtest",
			want:    &chaincfg.RegressionNetParams,
		},
		{
			network: "simnet",
			want:    &chaincfg.SimNetParams,
		},
		{
			network: "signet",
			want:    &chaincfg.SigNetParams,
		},
		{
			network: "fakenet",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.network, func(t *testing.T) {
			t.Parallel()

			got, err := networkToChainParams(tc.network)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Same(t, tc.want, got)
		})
	}
}

// TestNetworkToLndclient verifies network strings map to lndclient networks.
func TestNetworkToLndclient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		network string
		want    lndclient.Network
		wantErr bool
	}{
		{
			network: "mainnet",
			want:    lndclient.NetworkMainnet,
		},
		{
			network: "testnet",
			want:    lndclient.NetworkTestnet,
		},
		{
			network: "testnet4",
			want:    lndclient.NetworkTestnet4,
		},
		{
			network: "regtest",
			want:    lndclient.NetworkRegtest,
		},
		{
			network: "simnet",
			want:    lndclient.NetworkSimnet,
		},
		{
			network: "signet",
			want:    lndclient.NetworkSignet,
		},
		{
			network: "fakenet",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.network, func(t *testing.T) {
			t.Parallel()

			got, err := networkToLndclient(tc.network)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
