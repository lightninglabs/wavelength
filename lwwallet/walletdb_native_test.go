//go:build !js || !wasm

package lwwallet

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	btcwalletbase "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/wavelength/db"
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

		// existingEmpty leaves existingFile zero-length, the way a
		// SQLite open that failed before writing anything does.
		existingEmpty bool

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
		name:          "bolt with empty sqlite file",
		backend:       DBBackendBolt,
		existingFile:  sqliteWalletDBFileName,
		existingEmpty: true,
		wantFile:      btcwalletbase.WalletDBName,
	}, {
		name:          "sqlite with empty bolt file",
		backend:       DBBackendSQLite,
		existingFile:  btcwalletbase.WalletDBName,
		existingEmpty: true,
		wantErr:       btcwalletbase.WalletDBName,
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
				var contents []byte
				if !tc.existingEmpty {
					contents = []byte("database")
				}

				path := filepath.Join(dbDir, tc.existingFile)
				require.NoError(
					t, os.WriteFile(
						path, contents, 0600,
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

	// Check while the wallet is running, so the -wal sidecar is still
	// on disk; a clean shutdown removes it. Whether a -shm file exists
	// depends on the driver: one that applies locking_mode before
	// journal_mode keeps the WAL index in memory instead. If it is
	// there, it must not be readable by anyone else either.
	dbPath := filepath.Join(dbDir, sqliteWalletDBFileName)
	for _, path := range []string{
		dbPath, dbPath + "-wal", dbPath + "-shm",
	} {
		info, err := os.Stat(path)
		if path == dbPath+"-shm" && os.IsNotExist(err) {
			continue
		}
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
	driverName, dsn, err := sqliteWalletDBDSN(dbPath)
	require.NoError(t, err)
	walletDB, err := openSQLWalletDB(
		context.Background(), driverName, dsn,
	)
	require.NoError(t, err)
	require.NoError(t, walletDB.Close())
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

// TestSQLiteWalletDBDSNPragmas pins the settings the production DSN
// applies to every connection. They are the durability and integrity
// guarantees the SQLite backend rests on, and a driver silently
// dropping one of them would otherwise leave every other test green.
// It runs under both SQLite drivers the build can select.
func TestSQLiteWalletDBDSNPragmas(t *testing.T) {
	t.Parallel()

	driverName, dsn, err := sqliteWalletDBDSN(
		filepath.Join(t.TempDir(), sqliteWalletDBFileName),
	)
	require.NoError(t, err)

	raw, err := sql.Open(driverName, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, raw.Close())
	})

	// Every pragma is read on one connection, the way the single
	// connection of the wallet store sees them.
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	for pragma, want := range map[string]string{
		"foreign_keys": "1",
		"journal_mode": "wal",
		"synchronous":  "2",
		"fullfsync":    "1",
		"busy_timeout": "30000",
		"locking_mode": "exclusive",
	} {
		var got string
		err := conn.QueryRowContext(
			t.Context(), "PRAGMA "+pragma,
		).Scan(&got)
		require.NoError(t, err, pragma)
		require.Equal(t, want, strings.ToLower(got), pragma)
	}
}

// TestSQLiteWalletDBExcludesSecondOpener asserts that a running wallet
// keeps other connections out of its database, which is what stands in
// for BoltDB's file lock: a second daemon on the same directory must
// fail rather than derive the same addresses as the first.
func TestSQLiteWalletDBExcludesSecondOpener(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 11)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, seed[:], []byte("exclusive-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	w, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	// The second opener uses a short busy timeout so the test does not
	// sit out the production one; with the lock held for the wallet's
	// lifetime it would fail all the same, only later.
	driverName, dsn, err := db.SQLiteDSN(db.SQLiteOpenConfig{
		DatabaseFileName: filepath.Join(dbDir, sqliteWalletDBFileName),
		Pragmas: []db.SQLitePragma{{
			Name:  "busy_timeout",
			Value: "100",
		}},
	})
	require.NoError(t, err)

	raw, err := sql.Open(driverName, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, raw.Close())
	})

	var count int
	err = raw.QueryRowContext(
		t.Context(), "SELECT COUNT(*) FROM walletdb_kv",
	).Scan(&count)
	require.ErrorContains(t, err, "database is locked")
}

// TestSQLiteWalletDBCheckpoint asserts that the main SQLite database
// file holds the wallet on its own both right after it is created and
// after a clean shutdown. Until a checkpoint, the wallet lives in the
// -wal sidecar, so a process killed before one — the normal end of a
// mobile app — would otherwise leave a main file that a copy or backup
// taking only that file restores as "no wallet".
func TestSQLiteWalletDBCheckpoint(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 13)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, seed[:], []byte("checkpoint-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	// requireMainFileHoldsWallet copies the main database file alone
	// into a fresh directory and probes the copy.
	dbPath := filepath.Join(dbDir, sqliteWalletDBFileName)
	requireMainFileHoldsWallet := func() {
		t.Helper()

		contents, err := os.ReadFile(dbPath)
		require.NoError(t, err)

		copyDir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(copyDir, sqliteWalletDBFileName),
			contents, 0600,
		))

		copyCfg := cfg
		copyCfg.DBDir = copyDir
		exists, err := WalletExists(copyCfg)
		require.NoError(t, err)
		require.True(t, exists)
	}

	// Copy before the wallet ever starts or stops, which is the state a
	// process killed right after creating the wallet leaves behind.
	w, err := New(cfg)
	require.NoError(t, err)
	requireMainFileHoldsWallet()

	require.NoError(t, w.Start())
	w.Stop()

	// A clean shutdown folds the log back in and removes it.
	require.NoFileExists(t, dbPath+"-wal")
	requireMainFileHoldsWallet()
}

// TestSQLiteWalletDBEmptyFile covers the zero-length database file a
// SQLite open that failed before writing anything leaves behind. It
// holds no wallet: the probe must say so without writing a schema into
// it, and the operator must be able to create the wallet on either
// backend.
func TestSQLiteWalletDBEmptyFile(t *testing.T) {
	t.Parallel()

	for _, backend := range testWalletDBBackends {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()

			esplora := newTestEsplora(t)
			dbDir := t.TempDir()
			dbPath := filepath.Join(dbDir, sqliteWalletDBFileName)
			require.NoError(t, os.WriteFile(dbPath, nil, 0600))

			var seed [32]byte
			for i := range seed {
				seed[i] = byte(i + 17)
			}

			cfg := testWalletConfig(
				esplora.URL, dbDir, seed[:],
				[]byte("empty-password"),
			)
			cfg.DBBackend = backend

			exists, err := WalletExists(cfg)
			require.NoError(t, err)
			require.False(t, exists)

			info, err := os.Stat(dbPath)
			require.NoError(t, err)
			require.Zero(t, info.Size())

			w, err := New(cfg)
			require.NoError(t, err)
			require.NoError(t, w.Start())
			w.Stop()

			exists, err = WalletExists(cfg)
			require.NoError(t, err)
			require.True(t, exists)
		})
	}
}

// TestSQLiteWalletDBQuestionMarkPath asserts that a data directory with
// a '?' in its path is refused before anything is written. Both SQLite
// drivers split the DSN at the first '?', so the database would
// otherwise land at a truncated sibling path, outside the directory,
// without any of its settings.
func TestSQLiteWalletDBQuestionMarkPath(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	parentDir := t.TempDir()
	dbDir := filepath.Join(parentDir, "wallet?dir")

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 19)
	}

	cfg := testWalletConfig(
		esplora.URL, dbDir, seed[:], []byte("question-password"),
	)
	cfg.DBBackend = DBBackendSQLite

	_, err := New(cfg)
	require.ErrorContains(t, err, "contains '?'")

	entries, err := os.ReadDir(parentDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
