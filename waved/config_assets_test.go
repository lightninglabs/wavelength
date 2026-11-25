package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// testGroupKeyHex is a valid compressed public key: the secp256k1 generator.
const testGroupKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959" +
	"f2815b16f81798"

// testCourierAddr is a proof courier a version 2 address can name.
const testCourierAddr = "authmailbox+universerpc://tapd:10029"

// TestConfigValidateAssets checks that Taproot Assets are off by default and
// can be turned on only on regtest, on any wallet.
func TestConfigValidateAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		assets      *AssetsConfig
		network     string
		walletType  string
		wantEnabled bool
		wantErr     string
	}{
		{
			name:   "default disables assets",
			assets: defaultAssetsConfig(),
		},
		{
			name: "missing section disables assets",
		},
		{
			name: "enabled on the default wallet",
			assets: &AssetsConfig{
				Enabled: true,
			},
			wantEnabled: true,
		},
		{
			name: "enabled with a trusted group",
			assets: &AssetsConfig{
				Enabled: true,
				GroupKeys: []string{
					testGroupKeyHex,
				},
			},
			wantEnabled: true,
		},
		{
			name: "enabled with a malformed group key",
			assets: &AssetsConfig{
				Enabled: true,
				GroupKeys: []string{
					"02abcd",
				},
			},
			wantErr: "assets.groupkeys",
		},
		{
			name: "enabled on an lnd wallet",
			assets: &AssetsConfig{
				Enabled: true,
			},
			walletType:  WalletTypeLnd,
			wantEnabled: true,
		},
		{
			name: "enabled outside regtest",
			assets: &AssetsConfig{
				Enabled: true,
			},
			network: "signet",
			wantErr: "available on regtest only",
		},
		{
			name: "enabled with a proof courier",
			assets: &AssetsConfig{
				Enabled: true,
				Courier: AssetCourierConfig{
					Addr: testCourierAddr,
					Endpoints: []string{
						"tapd:10029=127.0.0.1:12029",
					},
				},
			},
			wantEnabled: true,
		},
		{
			name: "enabled with a courier of another kind",
			assets: &AssetsConfig{
				Enabled: true,
				Courier: AssetCourierConfig{
					Addr: "universerpc://tapd:10029",
				},
			},
			wantErr: "assets.courier.addr",
		},
		{
			name: "enabled with a malformed courier endpoint",
			assets: &AssetsConfig{
				Enabled: true,
				Courier: AssetCourierConfig{
					Endpoints: []string{
						"tapd:10029",
					},
				},
			},
			wantErr: "assets.courier.endpoints",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			if tc.network != "" {
				cfg.Network = tc.network
			}
			if tc.walletType != "" {
				cfg.Wallet.Type = tc.walletType
			}
			cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
			cfg.Assets = tc.assets

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, cfg.Assets.Enabled)
		})
	}
}

// TestAssetsDisabled checks that disabled assets install no verifier, so
// rounds with asset requests fail before signing.
func TestAssetsDisabled(t *testing.T) {
	t.Parallel()

	s := &Server{cfg: DefaultConfig()}

	cleanup, err := s.initEmbeddedAssets(t.Context(), nil)
	require.NoError(t, err)
	cleanup()
	require.Nil(t, s.assetVTXOVerifier)
	require.Nil(t, s.assetClient)
}

// TestAssetCourierAddr accepts a proof courier only as a full
// authmailbox+universerpc://host:port address, since the asset engine
// rejects any other scheme and completes no missing part.
func TestAssetCourierAddr(t *testing.T) {
	t.Parallel()

	none, err := (&AssetCourierConfig{}).addr()
	require.NoError(t, err)
	require.Nil(t, none)

	addr, err := (&AssetCourierConfig{Addr: testCourierAddr}).addr()
	require.NoError(t, err)
	require.Equal(t, testCourierAddr, addr.String())
	require.Equal(t, "tapd:10029", addr.Host)

	for _, malformed := range []string{
		"tapd:10029",
		"universerpc://tapd:10029",
		"hashmail://tapd:10029",
		"authmailbox+universerpc://tapd",
		"authmailbox+universerpc://:10029",
		"authmailbox+universerpc://tapd:10029/universe",
		"authmailbox+universerpc://tapd:10029?tls=off",
		"authmailbox+universerpc://user@tapd:10029",
		"authmailbox+universerpc:tapd:10029",
	} {
		_, err := (&AssetCourierConfig{Addr: malformed}).addr()
		require.ErrorContains(
			t, err, "assets.courier.addr", malformed,
		)
	}
}

// TestAssetCourierEndpoints parses the dial overrides of proof couriers.
func TestAssetCourierEndpoints(t *testing.T) {
	t.Parallel()

	endpoints, err := (&AssetCourierConfig{
		Endpoints: []string{
			"tapd:10029=127.0.0.1:12029",
			"universe.example.com:10029=[::1]:443=/etc/ca.pem",
			"pinned:10029==/etc/pinned.pem",
		},
	}).endpoints()
	require.NoError(t, err)
	require.Equal(t, map[string]assetCourierEndpoint{
		"tapd:10029": {
			target: "127.0.0.1:12029",
		},
		"universe.example.com:10029": {
			target:      "[::1]:443",
			tlsCertPath: "/etc/ca.pem",
		},
		"pinned:10029": {
			tlsCertPath: "/etc/pinned.pem",
		},
	}, endpoints)

	for _, malformed := range [][]string{
		{
			"tapd:10029",
		},
		{
			"tapd=127.0.0.1:12029",
		},
		{
			":10029=127.0.0.1:12029",
		},
		{
			"tapd:10029=127.0.0.1",
		},
		{
			"tapd:10029=",
		},
		{
			"tapd:10029==",
		},
		{
			"tapd:10029=127.0.0.1:1",
			"tapd:10029=127.0.0.1:2",
		},
	} {
		_, err := (&AssetCourierConfig{
			Endpoints: malformed,
		}).endpoints()
		require.ErrorContains(
			t, err, "assets.courier.endpoints", malformed,
		)
	}
}
