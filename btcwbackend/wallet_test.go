package btcwbackend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// TestWalletExistsRefusesSQLiteWallet asserts that the probe refuses a
// directory holding a wallet written by lwwallet's SQLite backend.
// btcwallet only knows the BoltDB file name, so it would otherwise see
// an empty directory and create a second wallet beside the existing one.
// The zero-length file a failed first SQLite open leaves behind holds no
// wallet and must not block anything.
func TestWalletExistsRefusesSQLiteWallet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		contents []byte
		wantErr  string
	}{{
		name:     "sqlite wallet",
		contents: []byte("database"),
		wantErr:  "lwwallet's SQLite backend",
	}, {
		name: "empty sqlite file",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dbDir := t.TempDir()
			sqlitePath, _, err := lwwallet.SQLiteWalletDBExists(
				dbDir,
			)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(
				sqlitePath, tc.contents, 0600,
			))

			exists, err := WalletExists(Config{
				Config: walletcore.Config{
					ChainParams: &chaincfg.
						RegressionNetParams,
					DBDir: dbDir,
				},
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.False(t, exists)
			require.NoFileExists(
				t, filepath.Join(dbDir, "wallet.db"),
			)
		})
	}
}
