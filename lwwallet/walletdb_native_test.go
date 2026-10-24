//go:build !js || !wasm

package lwwallet

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	btcwalletbase "github.com/btcsuite/btcwallet/wallet"
	"github.com/stretchr/testify/require"
)

// testWalletDBBackends lists the wallet database backends the platform
// supports, so backend-agnostic tests can assert the same contract for
// each of them.
var testWalletDBBackends = []string{DBBackendBolt, DBBackendSQLite}

// TestWalletDBPath asserts how the configured backend maps to a wallet
// database file, and in particular that a database left behind by the
// other backend is refused instead of ignored.
func TestWalletDBPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string

		// backend is the configured Config.DBBackend value.
		backend string

		// existingFile, when set, is created in the wallet
		// database directory before resolution runs.
		existingFile string

		// wantFile is the expected database file name.
		wantFile string

		// wantErr is a substring the error must contain.
		wantErr string
	}{{
		name:     "empty backend defaults to bolt",
		backend:  "",
		wantFile: btcwalletbase.WalletDBName,
	}, {
		name:     "bolt",
		backend:  DBBackendBolt,
		wantFile: btcwalletbase.WalletDBName,
	}, {
		name:     "sqlite",
		backend:  DBBackendSQLite,
		wantFile: sqliteWalletDBFileName,
	}, {
		name:         "bolt with existing bolt database",
		backend:      DBBackendBolt,
		existingFile: btcwalletbase.WalletDBName,
		wantFile:     btcwalletbase.WalletDBName,
	}, {
		name:         "sqlite with existing sqlite database",
		backend:      DBBackendSQLite,
		existingFile: sqliteWalletDBFileName,
		wantFile:     sqliteWalletDBFileName,
	}, {
		name:         "bolt with existing sqlite database",
		backend:      DBBackendBolt,
		existingFile: sqliteWalletDBFileName,
		wantErr:      sqliteWalletDBFileName,
	}, {
		name:         "sqlite with existing bolt database",
		backend:      DBBackendSQLite,
		existingFile: btcwalletbase.WalletDBName,
		wantErr:      btcwalletbase.WalletDBName,
	}, {
		name:    "unknown backend",
		backend: "postgres",
		wantErr: "unknown wallet database backend",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dbDir := t.TempDir()
			if tc.existingFile != "" {
				path := filepath.Join(dbDir, tc.existingFile)
				require.NoError(
					t, os.WriteFile(
						path, nil, 0600,
					),
				)
			}

			path, err := walletDBPath(Config{
				DBDir:     dbDir,
				DBBackend: tc.backend,
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(
				t, filepath.Join(dbDir, tc.wantFile), path,
			)
		})
	}
}

// TestSQLiteWalletDBPermissions asserts the wallet database and its WAL
// sidecars are owner-only. SQLite creates files with 0666 & ~umask,
// which under a typical umask would leave the encrypted seed and key
// state world-readable, where btcwallet's BoltDB file is 0600.
func TestSQLiteWalletDBPermissions(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 7)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, seed[:], []byte("perms-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	w, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start())

	// Check while the wallet is running, so the -wal and -shm
	// sidecars are still on disk; a clean shutdown removes them.
	dbPath := filepath.Join(dbDir, sqliteWalletDBFileName)
	for _, path := range []string{
		dbPath, dbPath + "-wal", dbPath + "-shm",
	} {
		info, err := os.Stat(path)
		require.NoError(t, err, "expected %s to exist", path)
		require.Equal(
			t, os.FileMode(0600), info.Mode().Perm(),
			"unexpected mode for %s", path,
		)
	}

	w.Stop()
}

// TestSQLiteWalletDBInterruptedCreate covers the state an interrupted
// first run leaves behind: the SQLite database file and its schema
// exist, but no wallet was ever committed inside them. The wallet probe
// has to agree with btcwallet's own here, because New pairs the two
// decisions — a probe that reported an existing wallet would refuse the
// operator's real seed and send the seedless path into btcwallet.New,
// which creates a wallet from a random seed when handed none.
func TestSQLiteWalletDBInterruptedCreate(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 5)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, nil, []byte("interrupted-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	// Open and close the wallet database without creating a wallet in
	// it, which is the on-disk state New leaves behind if it is killed
	// between opening the database and btcwallet committing the
	// wallet.
	dbPath, err := walletDBPath(cfg)
	require.NoError(t, err)
	db, err := openSQLWalletDB(
		context.Background(), sqliteWalletDBDriverName,
		sqliteWalletDBDSN(dbPath),
	)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.FileExists(t, dbPath)

	// The database file is there, so a file-presence probe would claim
	// a wallet exists. None does.
	exists, err := WalletExists(cfg)
	require.NoError(t, err)
	require.False(t, exists)

	// The seedless open path must fail loudly instead of minting a
	// wallet from a seed nobody has a copy of.
	_, err = New(cfg)
	require.ErrorIs(t, err, ErrWalletNotFound)

	// The operator who does hold the seed must be able to recover in
	// place, without deleting the database by hand first.
	cfg.Seed = seed[:]
	w, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	exists, err = WalletExists(cfg)
	require.NoError(t, err)
	require.True(t, exists)
}

// TestSQLiteWalletDBLayout asserts that the SQLite backend keeps the
// wallet entirely inside its own database file, and in particular that
// it leaves no BoltDB database behind: a caller switching back to the
// default backend must not find one and open an empty wallet from it.
func TestSQLiteWalletDBLayout(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 3)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, seed[:], []byte("sqlite-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	// A probe on a fresh directory must not create the database it is
	// asking about, or the guard against mixing up the two backends
	// would trip on a database that never held a wallet.
	exists, err := WalletExists(cfg)
	require.NoError(t, err)
	require.False(t, exists)

	entries, err := os.ReadDir(dbDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	w, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	w.Stop()

	require.FileExists(t, filepath.Join(dbDir, sqliteWalletDBFileName))
	require.NoFileExists(
		t, filepath.Join(dbDir, btcwalletbase.WalletDBName),
	)
}
