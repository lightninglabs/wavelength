package btcwbackend

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// TestDefaultFeeURL verifies mainnet resolves to the mainnet
// nodes.lightning.computer endpoint, testnet3/testnet4/signet share the
// testnet endpoint, and unsupported networks error out instead of silently
// returning an empty URL.
func TestDefaultFeeURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  *chaincfg.Params
		want    string
		wantErr bool
	}{
		{
			name:   "mainnet",
			params: &chaincfg.MainNetParams,
			want:   DefaultFeeURLMainnet,
		},
		{
			name:   "testnet3",
			params: &chaincfg.TestNet3Params,
			want:   DefaultFeeURLTestnet,
		},
		{
			name:   "testnet4",
			params: &chaincfg.TestNet4Params,
			want:   DefaultFeeURLTestnet,
		},
		{
			name:   "signet",
			params: &chaincfg.SigNetParams,
			want:   DefaultFeeURLTestnet,
		},
		{
			name:    "regtest has no default",
			params:  &chaincfg.RegressionNetParams,
			wantErr: true,
		},
		{
			name:    "simnet has no default",
			params:  &chaincfg.SimNetParams,
			wantErr: true,
		},
		{
			name:    "nil params",
			params:  nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DefaultFeeURL(tc.params)
			if tc.wantErr {
				require.Error(t, err)
				require.Empty(t, got)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
