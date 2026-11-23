package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// testGroupKeyHex is a valid compressed public key: the secp256k1 generator.
const testGroupKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959" +
	"f2815b16f81798"

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
