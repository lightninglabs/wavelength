package waved

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/stretchr/testify/require"
)

// TestConfigValidateWalletDBBackend checks that the wallet database
// backend selector only accepts the backends lwwallet implements, so a
// typo fails at startup instead of at wallet-unlock time, and that the
// SQLite backend is refused for wallet types that would silently ignore
// it.
func TestConfigValidateWalletDBBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		walletType string
		backend    string
		wantErr    string
	}{
		{
			name:       "unset keeps the default",
			walletType: WalletTypeLwwallet,
		},
		{
			name:       "bolt",
			walletType: WalletTypeLwwallet,
			backend:    lwwallet.DBBackendBolt,
		},
		{
			name:       "sqlite",
			walletType: WalletTypeLwwallet,
			backend:    lwwallet.DBBackendSQLite,
		},
		{
			name:       "unknown backend",
			walletType: WalletTypeLwwallet,
			backend:    "postgres",
			wantErr:    "unknown wallet.dbbackend",
		},
		{
			name:       "btcwallet keeps bolt",
			walletType: WalletTypeBtcwallet,
			backend:    lwwallet.DBBackendBolt,
		},
		{
			name:       "btcwallet refuses sqlite",
			walletType: WalletTypeBtcwallet,
			backend:    lwwallet.DBBackendSQLite,
			wantErr:    "only supported when wallet.type",
		},
		{
			name:       "btcwallet refuses unknown backend",
			walletType: WalletTypeBtcwallet,
			backend:    "postgres",
			wantErr:    "unknown wallet.dbbackend",
		},
		{
			name:       "lnd refuses sqlite",
			walletType: WalletTypeLnd,
			backend:    lwwallet.DBBackendSQLite,
			wantErr:    "only supported when wallet.type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.Wallet.Type = tc.walletType
			cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
			cfg.Wallet.FeeURL = "http://127.0.0.1:3001"
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

// TestAutoUnlockStopsOnProbeFailure asserts that a wallet database probe
// that cannot answer stops startup instead of leaving the daemon up with
// no wallet. A host that onboards whenever it sees that state would hand
// the user a fresh seed for a directory that may well hold a funded
// wallet, here one written by the other database backend.
func TestAutoUnlockStopsOnProbeFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.DataDir = t.TempDir()
	cfg.Wallet.Type = WalletTypeLwwallet
	cfg.Wallet.DBBackend = lwwallet.DBBackendSQLite

	require.NoError(t, os.MkdirAll(cfg.NetworkDir(), 0700))
	require.NoError(t, os.WriteFile(
		filepath.Join(cfg.NetworkDir(), "wallet.db"),
		[]byte("database"), 0600,
	))

	server := &Server{
		cfg:         cfg,
		chainParams: &chaincfg.RegressionNetParams,
		log:         btclog.Disabled,
	}

	err := server.tryAutoUnlockLwwallet(t.Context())
	require.ErrorContains(t, err, "probe wallet database")
	require.Equal(
		t, WalletStateNone, WalletState(server.walletState.Load()),
	)
}
