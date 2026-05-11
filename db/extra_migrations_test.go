package db

import (
	"embed"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/extra_migrations/*.sql
var testExtraMigrationsFS embed.FS

// TestExtraMigrationsSqlite verifies that WithExtraMigrations applies the
// downstream migration set against the same SQLite DB as darepo's core
// migrations, populates the table the migration creates, and tracks its own
// version in a separate schema_migrations_<Name> table.
//
// This exercise mirrors how lightninglabs/swapdk-server will use the hook to
// stack swap-server-specific tables onto darepo's chain_info / mailbox /
// rounds tables in a single shared database.
func TestExtraMigrationsSqlite(t *testing.T) {
	ctx := t.Context()

	dbFile := filepath.Join(t.TempDir(), "extra.db")
	store, err := NewSqliteStore(
		&SqliteConfig{DatabaseFileName: dbFile},
		btclog.Disabled,
		WithExtraMigrations(ExtraMigration{
			Name:          "swap_test",
			FS:            testExtraMigrationsFS,
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.DB.Close()) })

	// The extension-migration table should now exist alongside darepo's
	// core tables.
	_, err = store.DB.ExecContext(ctx, `
		INSERT INTO extra_migrations_test_table (id, note)
		VALUES (1, 'inserted by extra-migration test')
	`)
	require.NoError(t, err)

	var note string
	row := store.DB.QueryRowContext(ctx, `
		SELECT note FROM extra_migrations_test_table WHERE id = 1
	`)
	require.NoError(t, row.Scan(&note))
	require.Equal(t, "inserted by extra-migration test", note)

	// darepo's own version table is still populated, and the extension
	// uses its own bookkeeping table — the two version counters are
	// independent.
	var coreVersion uint
	row = store.DB.QueryRowContext(ctx, `
		SELECT version FROM schema_migrations
	`)
	require.NoError(t, row.Scan(&coreVersion))
	require.Equal(t, LatestMigrationVersion, coreVersion)

	var extraVersion uint
	row = store.DB.QueryRowContext(ctx, `
		SELECT version FROM schema_migrations_swap_test
	`)
	require.NoError(t, row.Scan(&extraVersion))
	require.Equal(t, uint(1), extraVersion)
}

// TestExtraMigrationsSkipCoreSqlite verifies that WithSkipCoreMigrations
// suppresses darepo's own schema while still applying the registered
// extension set. After the store opens, only the extension-owned table and
// the extension's bookkeeping row are present — chain_info, mailbox tables,
// and schema_migrations from darepo's core set are absent.
func TestExtraMigrationsSkipCoreSqlite(t *testing.T) {
	ctx := t.Context()

	dbFile := filepath.Join(t.TempDir(), "skip_core.db")
	store, err := NewSqliteStore(
		&SqliteConfig{DatabaseFileName: dbFile},
		btclog.Disabled,
		WithSkipCoreMigrations(),
		WithExtraMigrations(ExtraMigration{
			Name:          "swap_test",
			FS:            testExtraMigrationsFS,
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.DB.Close()) })

	// The extension-migration table materialized despite the core
	// migrations being skipped.
	_, err = store.DB.ExecContext(ctx, `
		INSERT INTO extra_migrations_test_table (id, note)
		VALUES (1, 'inserted with core migrations skipped')
	`)
	require.NoError(t, err)

	// darepo's core schema_migrations table must not exist — that is the
	// load-bearing guarantee of WithSkipCoreMigrations.
	var coreCount int
	row := store.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations'
	`)
	require.NoError(t, row.Scan(&coreCount))
	require.Equal(t, 0, coreCount, "core schema_migrations should be absent")

	// Likewise, a representative core table (chain_info) must not exist.
	var chainInfoCount int
	row = store.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'chain_info'
	`)
	require.NoError(t, row.Scan(&chainInfoCount))
	require.Equal(t, 0, chainInfoCount, "core chain_info should be absent")

	// The extension counter is still populated under its own bookkeeping
	// table so re-opens are idempotent.
	var extraVersion uint
	row = store.DB.QueryRowContext(ctx, `
		SELECT version FROM schema_migrations_swap_test
	`)
	require.NoError(t, row.Scan(&extraVersion))
	require.Equal(t, uint(1), extraVersion)
}

// TestExtraMigrationsRejectsBadName verifies the validate() preflight catches
// names that would produce SQL-unsafe migration table identifiers.
func TestExtraMigrationsRejectsBadName(t *testing.T) {
	cases := []ExtraMigration{
		// Empty name.
		{
			Name:          "",
			FS:            testExtraMigrationsFS,
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		},
		// Starts with a digit.
		{
			Name:          "1bad",
			FS:            testExtraMigrationsFS,
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		},
		// Embedded SQL injection attempt.
		{
			Name:          "swap; DROP TABLE x",
			FS:            testExtraMigrationsFS,
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		},
		// Missing FS.
		{
			Name:          "noFS",
			Path:          "testdata/extra_migrations",
			LatestVersion: 1,
		},
		// Missing Path.
		{
			Name:          "noPath",
			FS:            testExtraMigrationsFS,
			LatestVersion: 1,
		},
		// Zero LatestVersion.
		{
			Name: "noVersion",
			FS:   testExtraMigrationsFS,
			Path: "testdata/extra_migrations",
		},
	}

	for _, ex := range cases {
		err := ex.validate()
		require.Error(t, err, "expected %q to fail validate()", ex.Name)
	}
}

// TestExtraMigrationsAdditive verifies WithExtraMigrations is additive across
// multiple calls — a downstream consumer can stack registrations without
// callers needing to gather them up front.
func TestExtraMigrationsAdditive(t *testing.T) {
	ex1 := ExtraMigration{
		Name:          "first",
		FS:            testExtraMigrationsFS,
		Path:          "testdata/extra_migrations",
		LatestVersion: 1,
	}
	ex2 := ExtraMigration{
		Name:          "second",
		FS:            testExtraMigrationsFS,
		Path:          "testdata/extra_migrations",
		LatestVersion: 1,
	}

	so := collectStoreOpts([]StoreOption{
		WithExtraMigrations(ex1),
		WithExtraMigrations(ex2),
	})

	require.Len(t, so.extras, 2)
	require.Equal(t, "first", so.extras[0].Name)
	require.Equal(t, "second", so.extras[1].Name)
}

// TestExtraMigrationsPrevalidate verifies that a malformed entry later in the
// extras slice prevents *any* DDL from being applied — earlier entries must
// not be partially materialized when a sibling registration fails validation.
func TestExtraMigrationsPrevalidate(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prevalidate.db")

	// First descriptor is valid; second has an empty Name which fails
	// the SQL-safe identifier regex. The whole batch must be rejected
	// before either is applied.
	_, err := NewSqliteStore(
		&SqliteConfig{DatabaseFileName: dbFile},
		btclog.Disabled,
		WithExtraMigrations(
			ExtraMigration{
				Name:          "good",
				FS:            testExtraMigrationsFS,
				Path:          "testdata/extra_migrations",
				LatestVersion: 1,
			},
			ExtraMigration{
				Name:          "",
				FS:            testExtraMigrationsFS,
				Path:          "testdata/extra_migrations",
				LatestVersion: 1,
			},
		),
	)
	require.Error(t, err)

	// Reopen the same file with no extension migrations and confirm
	// neither set's bookkeeping table was created — proof that the
	// malformed entry short-circuited the run before any DDL.
	store, err := NewSqliteStore(
		&SqliteConfig{DatabaseFileName: dbFile},
		btclog.Disabled,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.DB.Close()) })

	var goodCount int
	row := store.DB.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations_good'
	`)
	require.NoError(t, row.Scan(&goodCount))
	require.Equal(
		t, 0, goodCount,
		"valid entry must not have materialized DDL after a "+
			"sibling failed preflight",
	)
}
