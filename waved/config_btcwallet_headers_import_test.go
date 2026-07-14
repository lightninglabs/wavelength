package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidateBtcwalletHeadersImport checks that btcwallet header import
// sources are either configured as a complete pair or omitted.
func TestConfigValidateBtcwalletHeadersImport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		blockSource  string
		filterSource string
		wantErr      string
	}{
		{
			name: "unset",
		},
		{
			name:         "both set",
			blockSource:  "blocks.bin",
			filterSource: "filters.bin",
		},
		{
			name:        "block only",
			blockSource: "blocks.bin",
			wantErr:     "must be specified together",
		},
		{
			name:         "filter only",
			filterSource: "filters.bin",
			wantErr:      "must be specified together",
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Wallet.Type = WalletTypeBtcwallet
			cfg.Wallet.FeeURL = "http://127.0.0.1:3001"
			cfg.Wallet.BtcwBlockSource = tc.blockSource
			cfg.Wallet.BtcwFilterSource = tc.filterSource

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
