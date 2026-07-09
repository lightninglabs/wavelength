package darepod

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

			if tc.name == "signet" {
				require.Equal(
					t, defaultSignetServerGRPCHost,
					cfg.Server.Host,
				)
				require.Equal(
					t, defaultSignetSwapServerGRPCHost,
					cfg.Swap.ServerAddress,
				)
			}
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

// TestApplyNetworkEndpointDefaults verifies signet selects the public staging
// deployment for both outbound transports while every other network keeps the
// local development endpoints.
func TestApplyNetworkEndpointDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      string
		transport    string
		wantArkHost  string
		wantSwapHost string
	}{
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
			name:         "regtest",
			network:      "regtest",
			transport:    RPCTransportGRPC,
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

			cfg.applyNetworkEndpointDefaults()

			require.Equal(t, tc.wantArkHost, cfg.Server.Host)
			require.Equal(
				t, tc.wantSwapHost, cfg.Swap.ServerAddress,
			)
		})
	}
}

// TestApplyNetworkEndpointDefaultsPreservesOverrides verifies signet does not
// replace custom endpoints or an explicitly insecure local development setup.
func TestApplyNetworkEndpointDefaultsPreservesOverrides(t *testing.T) {
	t.Parallel()

	t.Run("custom endpoints", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "signet"
		cfg.Server.Host = "ark.example.com:443"
		cfg.Swap.ServerAddress = "swap.example.com:443"

		cfg.applyNetworkEndpointDefaults()

		require.Equal(t, "ark.example.com:443", cfg.Server.Host)
		require.Equal(
			t, "swap.example.com:443", cfg.Swap.ServerAddress,
		)
	})

	t.Run("insecure local endpoints", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "signet"
		cfg.Server.Insecure = true
		cfg.Swap.ServerInsecure = true

		cfg.applyNetworkEndpointDefaults()

		require.Equal(t, DefaultServerHost, cfg.Server.Host)
		require.Equal(t, defaultSwapServerHost, cfg.Swap.ServerAddress)
	})
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
