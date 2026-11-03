package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// TestLiveSQLiteReportReadOnly verifies reports read committed WAL state
// without taking the daemon's write lock, and cannot modify the source.
func TestLiveSQLiteReportReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waved.db")
	writer, err := db.NewStoreFromConfig(&db.Config{
		Backend: "sqlite",
		Sqlite: &db.SqliteConfig{
			DatabaseFileName: path,
		},
	}, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	insert := `INSERT INTO ledger_entries
		(debit_account, credit_account, amount_sat, event_type,
			description, created_at)
		VALUES ('wallet_balance', 'opening_balance', 100,
			'wallet_utxo_created', 'fixture', 1000)`
	_, err = writer.DB().ExecContext(ctx, insert)
	require.NoError(t, err)

	cfg := &config{backend: "sqlite", sqliteDBFile: path}
	reader, err := openStore(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	// A live, uncommitted write must not prevent a report or leak its
	// pending row into the report's committed snapshot.
	writeTx, err := writer.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		_ = writeTx.Rollback()
	}()
	_, err = writeTx.ExecContext(ctx, insert)
	require.NoError(t, err)
	rep, err := buildReport(ctx, reader, cfg, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), rep.EntryCount)
	require.NoError(t, writeTx.Commit())

	// Once a reader has pinned a WAL snapshot, the daemon can still
	// commit. All report projections continue to see the same snapshot.
	readTx, err := reader.BaseDB().BeginTx(ctx, db.ReadTxOption())
	require.NoError(t, err)
	defer func() {
		_ = readTx.Rollback()
	}()
	queries := reader.Queries().WithTx(readTx)
	before, err := queries.GetClientLedgerStats(ctx)
	require.NoError(t, err)
	_, err = writer.DB().ExecContext(ctx, insert)
	require.NoError(t, err)
	after, err := queries.GetClientLedgerStats(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), before.EntryCount)
	require.Equal(t, before, after)
	require.NoError(t, readTx.Rollback())

	_, err = reader.DB().ExecContext(ctx, "DELETE FROM ledger_entries")
	require.Error(t, err)
	rep, err = buildReport(ctx, reader, cfg, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), rep.EntryCount)
}

// TestSQLiteReportEscapesFilename verifies mode=ro still addresses the exact
// file when its native path contains URI query or fragment characters.
func TestSQLiteReportEscapesFilename(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "original.db")
	writer, err := db.NewStoreFromConfig(&db.Config{
		Backend: "sqlite",
		Sqlite:  &db.SqliteConfig{DatabaseFileName: path},
	}, btclog.Disabled)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	escaped := filepath.Join(root, "report #?.db")
	require.NoError(t, os.Rename(path, escaped))
	cfg := &config{backend: "sqlite", sqliteDBFile: escaped}
	reader, err := openStore(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	rep, err := buildReport(t.Context(), reader, cfg, nil)
	require.NoError(t, err)
	require.NotEmpty(t, rep.Accounts)
}

// TestOpenStoreRequiresExistingSqliteFile verifies that the report command
// refuses to create a new sqlite database when the operator passes a path
// that does not exist.
func TestOpenStoreRequiresExistingSqliteFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")

	store, err := openStore(&config{
		backend:      "sqlite",
		sqliteDBFile: dbPath,
	})
	require.Error(t, err)
	require.Nil(t, store)
	require.NoFileExists(t, dbPath)
}

// TestOpenStoreValidatesBackend verifies the backend selector is validated and
// that the sqlite backend requires a database file.
func TestOpenStoreValidatesBackend(t *testing.T) {
	t.Run("unknown backend", func(t *testing.T) {
		store, err := openStore(&config{backend: "mysql"})
		require.Error(t, err)
		require.Nil(t, store)
	})

	t.Run("sqlite requires dbfile", func(t *testing.T) {
		store, err := openStore(&config{backend: "sqlite"})
		require.Error(t, err)
		require.Nil(t, store)
	})
}

// TestBuildReportReadsSeededAccounts migrates a fresh sqlite database, then
// opens it through the read-only report path and verifies buildReport returns
// the seeded chart of accounts with no ledger entries. This exercises the
// shared db opener, the read-only transaction, and the report assembly.
func TestBuildReportReadsSeededAccounts(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "waved.db")

	// Create and migrate a fresh database so the chart of accounts is
	// seeded.
	migrated, err := db.NewStoreFromConfig(&db.Config{
		Backend: "sqlite",
		Sqlite: &db.SqliteConfig{
			DatabaseFileName: dbFile,
			SkipMigrations:   false,
		},
		Postgres: &db.PostgresConfig{},
	}, btclog.Disabled)
	require.NoError(t, err)
	require.NoError(t, migrated.Close())

	cfg := &config{
		backend:      "sqlite",
		sqliteDBFile: dbFile,
		fiatCurrency: "usd",
	}

	store, err := openStore(cfg)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close())
	}()

	rep, err := buildReport(t.Context(), store, cfg, nil)
	require.NoError(t, err)

	require.Equal(t, "sqlite", rep.Backend)
	require.Equal(t, int64(0), rep.EntryCount)

	// The chart of accounts is seeded by migration, so every account is
	// reported even with zero ledger activity.
	require.NotEmpty(t, rep.Accounts)
	accountIDs := make(map[string]struct{}, len(rep.Accounts))
	for _, account := range rep.Accounts {
		accountIDs[account.AccountID] = struct{}{}
	}
	require.Contains(t, accountIDs, "wallet_clearing")
	require.Contains(t, accountIDs, "wallet_balance")

	// The checker shares the default opener. Its full snapshot must also
	// work through the read-only handle without hidden initialization
	// writes.
	checked, err := buildCheckReport(t.Context(), store, cfg)
	require.NoError(t, err)
	require.True(t, checked.Passed)
}

// TestWriteCSVReport verifies the CSV output emits a header plus one flat row
// per account and event, discriminated by the leading category column.
func TestWriteCSVReport(t *testing.T) {
	fiat := 12.5
	rep := &report{
		Backend:    "sqlite",
		EntryCount: 3,
		Accounts: []accountBalance{
			{
				AccountID:   "wallet_balance",
				AccountName: "Wallet Balance",
				AccountType: "asset",
				BalanceSat:  100_000,
				BalanceFiat: &fiat,
			},
			{
				AccountID:   "wallet_clearing",
				AccountName: "Wallet Clearing",
				AccountType: "asset",
				BalanceSat:  0,
			},
		},
		Events: []eventTotal{
			{
				EventType:  "boarding_fee_paid",
				EntryCount: 2,
				TotalSat:   5_000,
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, writeCSVReport(&buf, rep))

	rows, err := csv.NewReader(&buf).ReadAll()
	require.NoError(t, err)

	// Header + 2 accounts + 1 event.
	require.Len(t, rows, 4)
	require.Equal(t, []string{
		"category", "id", "name", "account_type", "entry_count",
		"amount_sat", "fiat",
	}, rows[0])

	// First account row carries its fiat value; the zero-balance account
	// leaves fiat empty.
	require.Equal(t, "account", rows[1][0])
	require.Equal(t, "wallet_balance", rows[1][1])
	require.Equal(t, "100000", rows[1][5])
	require.Equal(t, "12.50", rows[1][6])

	require.Equal(t, "account", rows[2][0])
	require.Equal(t, "wallet_clearing", rows[2][1])

	// Event rows carry the entry count and total, with no account type or
	// fiat.
	require.Equal(t, "event", rows[3][0])
	require.Equal(t, "boarding_fee_paid", rows[3][1])
	require.Equal(t, "2", rows[3][4])
	require.Equal(t, "5000", rows[3][5])
}
