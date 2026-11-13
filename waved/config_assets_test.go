package waved

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateAssets checks that the asset backend selector accepts
// only the implemented backends and that tapd requires an address.
func TestConfigValidateAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		assets      *AssetsConfig
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
				Backend: "embedded",
			},
			wantErr: "unknown assets.backend",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
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
