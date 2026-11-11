package waved

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testGroupKeyHex is a valid compressed public key: the secp256k1 generator.
const testGroupKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959" +
	"f2815b16f81798"

// TestConfigValidateAssets checks that the asset backend selector accepts
// only the implemented backends, that tapd requires an address, and that
// the embedded backend requires a local wallet on regtest.
func TestConfigValidateAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		assets      *AssetsConfig
		network     string
		walletType  string
		wantBackend string
		wantErr     string
	}{
		{
			name:        "default disables assets",
			assets:      defaultAssetsConfig(),
			wantBackend: AssetBackendNone,
		},
		{
			name:        "missing section disables assets",
			wantBackend: AssetBackendNone,
		},
		{
			name: "empty backend disables assets",
			assets: &AssetsConfig{
				Tapd: &TapdConfig{},
			},
			wantBackend: AssetBackendNone,
		},
		{
			name: "tapd",
			assets: &AssetsConfig{
				Backend: AssetBackendTapd,
				Tapd: &TapdConfig{
					Host: "localhost:10029",
				},
			},
			wantBackend: AssetBackendTapd,
		},
		{
			name: "tapd without host",
			assets: &AssetsConfig{
				Backend: AssetBackendTapd,
				Tapd:    &TapdConfig{},
			},
			wantErr: "assets.tapd.host is required",
		},
		{
			name: "tapd with negative timeout",
			assets: &AssetsConfig{
				Backend: AssetBackendTapd,
				Tapd: &TapdConfig{
					Host:       "localhost:10029",
					RPCTimeout: -time.Second,
				},
			},
			wantErr: "assets.tapd.rpctimeout",
		},
		{
			name: "unknown backend",
			assets: &AssetsConfig{
				Backend: "universe",
			},
			wantErr: "unknown assets.backend",
		},
		{
			name: "embedded on the default wallet",
			assets: &AssetsConfig{
				Backend: AssetBackendEmbedded,
			},
			wantBackend: AssetBackendEmbedded,
		},
		{
			name: "embedded with a trusted group",
			assets: &AssetsConfig{
				Backend: AssetBackendEmbedded,
				GroupKeys: []string{
					testGroupKeyHex,
				},
			},
			wantBackend: AssetBackendEmbedded,
		},
		{
			name: "embedded with a malformed group key",
			assets: &AssetsConfig{
				Backend: AssetBackendEmbedded,
				GroupKeys: []string{
					"02abcd",
				},
			},
			wantErr: "assets.groupkeys",
		},
		{
			name: "embedded on an lnd wallet",
			assets: &AssetsConfig{
				Backend: AssetBackendEmbedded,
			},
			walletType: WalletTypeLnd,
			wantErr:    "requires wallet.type",
		},
		{
			name: "embedded outside regtest",
			assets: &AssetsConfig{
				Backend: AssetBackendEmbedded,
			},
			network: "signet",
			wantErr: "available on regtest only",
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
			require.Equal(t, tc.wantBackend, cfg.Assets.Backend)
		})
	}
}

// TestConnectAssetBackendNone checks that disabled assets install no
// verifier, so rounds with asset requests fail before signing.
func TestConnectAssetBackendNone(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	s := &Server{cfg: cfg}

	verifier, cleanup, err := s.connectAssetBackend(t.Context())
	require.NoError(t, err)
	require.Nil(t, verifier)
	cleanup()
}
