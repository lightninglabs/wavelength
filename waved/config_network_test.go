package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/lwwallet"
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

// TestConfigValidateMainnetInsecureRPC verifies that rpc.notls and
// rpc.no-macaroons are refused on mainnet TCP listeners by default, and
// permitted once the operator opts in with allow-insecure-mainnet.
func TestConfigValidateMainnetInsecureRPC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		noTLS       bool
		noMacaroons bool
		override    bool
		wantErr     string
	}{
		{
			name:    "notls refused by default",
			noTLS:   true,
			wantErr: "rpc.notls cannot be used",
		},
		{
			name:        "no-macaroons refused by default",
			noMacaroons: true,
			wantErr:     "rpc.no-macaroons cannot be used",
		},
		{
			name:     "notls allowed with override",
			noTLS:    true,
			override: true,
		},
		{
			name:        "no-macaroons allowed with override",
			noMacaroons: true,
			override:    true,
		},
		{
			name:        "both allowed with override",
			noTLS:       true,
			noMacaroons: true,
			override:    true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "mainnet"
			cfg.AllowMainnet = true
			cfg.RPC.NoTLS = tc.noTLS
			cfg.RPC.NoMacaroons = tc.noMacaroons
			cfg.AllowInsecureMainnet = tc.override

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestConfigValidateWalletDefaults verifies Validate fills in the
// network-default Esplora/fee URL for the lwwallet and btcwallet backends
// when left empty, and still requires an explicit value on networks with no
// public default (regtest, simnet).
func TestConfigValidateWalletDefaults(t *testing.T) {
	t.Parallel()

	t.Run("lwwallet defaults per network", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			network string
			want    string
		}{
			{
				network: "testnet",
				want:    lwwallet.DefaultEsploraURLTestnet3,
			},
			{
				network: "testnet4",
				want:    lwwallet.DefaultEsploraURLTestnet4,
			},
			{
				network: "signet",
				want:    lwwallet.DefaultEsploraURLSignet,
			},
		}

		for _, tc := range tests {
			tc := tc
			t.Run(tc.network, func(t *testing.T) {
				t.Parallel()

				cfg := DefaultConfig()
				cfg.Network = tc.network

				require.NoError(t, cfg.Validate())
				require.Equal(t, tc.want, cfg.Wallet.EsploraURL)
			})
		}
	})

	t.Run("lwwallet mainnet default", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "mainnet"
		cfg.AllowMainnet = true

		require.NoError(t, cfg.Validate())
		require.Equal(
			t, lwwallet.DefaultEsploraURLMainnet,
			cfg.Wallet.EsploraURL,
		)
	})

	t.Run(
		"lwwallet regtest and simnet require explicit URL",
		func(t *testing.T) {
			t.Parallel()

			for _, network := range []string{"regtest", "simnet"} {
				network := network
				t.Run(network, func(t *testing.T) {
					t.Parallel()

					cfg := DefaultConfig()
					cfg.Network = network

					require.ErrorContains(
						t, cfg.Validate(),
						"wallet.esploraurl",
					)
				})
			}
		},
	)

	t.Run("btcwallet defaults per network", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			network string
			want    string
		}{
			{
				network: "testnet",
				want:    btcwbackend.DefaultFeeURLTestnet,
			},
			{
				network: "testnet4",
				want:    btcwbackend.DefaultFeeURLTestnet,
			},
			{
				network: "signet",
				want:    btcwbackend.DefaultFeeURLTestnet,
			},
		}

		for _, tc := range tests {
			tc := tc
			t.Run(tc.network, func(t *testing.T) {
				t.Parallel()

				cfg := DefaultConfig()
				cfg.Network = tc.network
				cfg.Wallet.Type = WalletTypeBtcwallet

				require.NoError(t, cfg.Validate())
				require.Equal(t, tc.want, cfg.Wallet.FeeURL)
			})
		}
	})

	t.Run("btcwallet mainnet default", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.Network = "mainnet"
		cfg.AllowMainnet = true
		cfg.Wallet.Type = WalletTypeBtcwallet

		require.NoError(t, cfg.Validate())
		require.Equal(
			t, btcwbackend.DefaultFeeURLMainnet, cfg.Wallet.FeeURL,
		)
	})

	t.Run(
		"btcwallet regtest and simnet require explicit URL",
		func(t *testing.T) {
			t.Parallel()

			for _, network := range []string{"regtest", "simnet"} {
				network := network
				t.Run(network, func(t *testing.T) {
					t.Parallel()

					cfg := DefaultConfig()
					cfg.Network = network
					cfg.Wallet.Type = WalletTypeBtcwallet

					require.ErrorContains(
						t, cfg.Validate(),
						"wallet.feeurl",
					)
				})
			}
		},
	)
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
