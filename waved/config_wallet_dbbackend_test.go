package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/stretchr/testify/require"
)

// TestConfigValidateWalletDBBackend checks that the lwwallet database
// backend selector only accepts the backends lwwallet implements, so a
// typo fails at startup instead of at wallet-unlock time.
func TestConfigValidateWalletDBBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend string
		wantErr string
	}{
		{
			name: "unset keeps the default",
		},
		{
			name:    "bolt",
			backend: lwwallet.DBBackendBolt,
		},
		{
			name:    "sqlite",
			backend: lwwallet.DBBackendSQLite,
		},
		{
			name:    "unknown backend",
			backend: "postgres",
			wantErr: "unknown wallet.dbbackend",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Wallet.Type = WalletTypeLwwallet
			cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
			cfg.Wallet.DBBackend = tc.backend

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
