package lwwallet

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// TestDefaultEsploraURL verifies each supported network resolves to its
// public mempool.space Esplora endpoint, and unsupported networks error out
// instead of silently returning an empty URL.
func TestDefaultEsploraURL(t *testing.T) {
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
			want:   DefaultEsploraURLMainnet,
		},
		{
			name:   "testnet3",
			params: &chaincfg.TestNet3Params,
			want:   DefaultEsploraURLTestnet3,
		},
		{
			name:   "testnet4",
			params: &chaincfg.TestNet4Params,
			want:   DefaultEsploraURLTestnet4,
		},
		{
			name:   "signet",
			params: &chaincfg.SigNetParams,
			want:   DefaultEsploraURLSignet,
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

			got, err := DefaultEsploraURL(tc.params)
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
