//go:build (!js || !wasm) && sqlite_cgo

package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestCGOSQLiteNumberedParams guards the lexical boundary of the adapter:
// parameter-looking text in literals, identifiers, and comments is data.
func TestCGOSQLiteNumberedParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{
			"$3, $1, $3, $2",
			"?3, ?1, ?3, ?2",
		},
		{
			"'cost $1' || $2",
			"'cost $1' || ?2",
		},
		{
			"'it''s $1', $2",
			"'it''s $1', ?2",
		},
		{
			"\"$1\", `$2`, [$3], $4",
			"\"$1\", `$2`, [$3], ?4",
		},
		{
			"-- $1\n$2 /* $3 */ $4",
			"-- $1\n?2 /* $3 */ ?4",
		},
		{"account$1, $name, :1, ?2, $3::suffix(foo), $4abc, $5",
			"account$1, $name, :1, ?2, $3::suffix(foo), $4abc, ?5"},
		{
			"'$1",
			"'$1",
		},
		{
			"/* $1",
			"/* $1",
		},
	}
	for _, test := range tests {
		require.Equal(t, test.want, sqliteNumberedParams(test.input))
	}
}

// newCGOSQLiteTestDB opens a real file with a short lock wait for contention
// tests. The public opener ensures each pooled connection uses the adapter.
func newCGOSQLiteTestDB(t *testing.T, path string, immediate bool) *sql.DB {
	t.Helper()

	result, err := OpenSQLiteDatabase(SQLiteOpenConfig{
		DatabaseFileName: path,
		Pragmas: []SQLitePragma{
			{Name: "foreign_keys", Value: "on"},
			{Name: "journal_mode", Value: "WAL"},
			{Name: "busy_timeout", Value: "10"},
			{Name: "synchronous", Value: "full"},
		},
		TxLockImmediate: immediate,
		MaxOpenConns:    2,
		MaxIdleConns:    2,
	})
	require.NoError(t, err)
	require.Equal(t, cgoSQLiteDriverName, result.DriverName)
	t.Cleanup(func() { require.NoError(t, result.DB.Close()) })

	return result.DB
}

// newCGOSQLiteTestFile creates the production schema before contention and
// reopen tests. It also exercises the CGO migration adapter and Go callbacks.
func newCGOSQLiteTestFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "client.db")
	store, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: path,
	}, btclog.Disabled)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	return path
}

// TestCGOSQLiteBindingsAndReopen exercises reordered/repeated numbered
// arguments through exec, query, transactions, and prepared statements. The
// SQL comes from the existing generated queries; no schema changes are needed.
func TestCGOSQLiteBindingsAndReopen(t *testing.T) {
	t.Parallel()

	path := newCGOSQLiteTestFile(t)
	handle := newCGOSQLiteTestDB(t, path, true)
	ctx := t.Context()

	// Swap the ordinal labels in a generated query to expose mattn's
	// appearance-order binding. $3 also occurs twice in this upsert.
	query := strings.NewReplacer("$1", "$3", "$3", "$1").Replace(
		sqlc.UpsertChainInfo,
	)
	_, err := handle.ExecContext(ctx, query, []byte{1}, "test", 101)
	require.NoError(t, err)

	tx, err := handle.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	stmt, err := tx.PrepareContext(ctx, query)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stmt.Close())
	}()
	_, err = stmt.ExecContext(ctx, []byte{2}, "test", 101)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// A higher ordinal with an unused lower argument exercises the query
	// path, independently of the order used for insertion.
	lookup := strings.ReplaceAll(sqlc.GetChainInfo, "$1", "$2")
	var row sqlc.ChainInfo
	err = handle.QueryRowContext(ctx, lookup, "unused", "test").Scan(
		&row.ID, &row.ChainName, &row.GenesisHash,
	)
	require.NoError(t, err)
	require.EqualValues(t, 101, row.ID)
	require.Equal(t, []byte{2}, row.GenesisHash)
	require.NoError(t, handle.Close())

	reopened := newCGOSQLiteTestDB(t, path, true)
	persisted, err := sqlc.NewSqlite(reopened).GetChainInfo(ctx, "test")
	require.NoError(t, err)
	require.Equal(t, row, persisted)

	// A real duplicate verifies that driver error extraction, rather than
	// just a synthetic error code, preserves constraint classification.
	params := sqlc.InsertMacaroonRootKeyParams{
		ID: []byte{
			1,
		}, RootKey: []byte{
			2,
		},
	}
	queries := sqlc.NewSqlite(reopened)
	require.NoError(t, queries.InsertMacaroonRootKey(ctx, params))
	err = queries.InsertMacaroonRootKey(ctx, params)
	require.Error(t, err)
	require.IsType(t, &ErrSQLUniqueConstraintViolation{}, MapSQLError(err))
}

// TestCGOSQLiteContention verifies immediate transactions contend at BEGIN,
// and a stale WAL read snapshot is classified for transaction restart.
func TestCGOSQLiteContention(t *testing.T) {
	t.Parallel()

	path := newCGOSQLiteTestFile(t)
	immediate := newCGOSQLiteTestDB(t, path, true)
	ctx := t.Context()
	tx, err := immediate.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = immediate.BeginTx(ctx, nil)
	require.Error(t, err)
	require.IsType(t, &ErrSerializationError{}, MapSQLError(err))
	require.NoError(t, tx.Rollback())
	require.NoError(t, immediate.Close())

	deferred := newCGOSQLiteTestDB(t, path, false)
	reader, err := deferred.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Rollback() })
	_, err = sqlc.NewSqlite(reader).ListChainInfo(ctx)
	require.NoError(t, err)

	// A second connection can commit while the reader keeps its snapshot
	// only when the WAL configuration is actually effective.
	params := sqlc.UpsertChainInfoParams{
		ID: 101, ChainName: "new", GenesisHash: []byte{
			1,
		},
	}
	err = sqlc.NewSqlite(deferred).UpsertChainInfo(ctx, params)
	require.NoError(t, err)
	err = sqlc.NewSqlite(reader).UpsertChainInfo(ctx, params)
	require.Error(t, err)
	require.IsType(t, &ErrDeadlockError{}, MapSQLError(err))
}

// TestCGOSQLiteErrorMapping covers extended result codes and wrapped errors
// so transaction retries and idempotent writes retain their classifications.
func TestCGOSQLiteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code sqlite3.ErrNo
		ext  sqlite3.ErrNoExtended
		want error
	}{
		{sqlite3.ErrConstraint, sqlite3.ErrConstraintUnique,
			&ErrSQLUniqueConstraintViolation{}},
		{sqlite3.ErrConstraint, sqlite3.ErrConstraintPrimaryKey,
			&ErrSQLUniqueConstraintViolation{}},
		{
			sqlite3.ErrBusy,
			0,
			&ErrSerializationError{},
		},
		{
			sqlite3.ErrBusy,
			sqlite3.ErrBusySnapshot,
			&ErrDeadlockError{},
		},
		{
			sqlite3.ErrLocked,
			0,
			&ErrDeadlockError{},
		},
		{sqlite3.ErrLocked, sqlite3.ErrLockedSharedCache,
			&ErrDeadlockError{}},
	}
	for _, test := range tests {
		original := sqlite3.Error{
			Code:         test.code,
			ExtendedCode: test.ext,
		}
		mapped := MapSQLError(fmt.Errorf("wrapped: %w", original))
		require.IsType(t, test.want, mapped)
		require.ErrorIs(t, mapped, original)
	}

	unknown := sqlite3.Error{Code: sqlite3.ErrConstraint,
		ExtendedCode: sqlite3.ErrConstraintForeignKey}
	require.ErrorIs(t, MapSQLError(unknown), unknown)
	unrelated := errors.New("unrelated")
	require.ErrorIs(t, MapSQLError(unrelated), unrelated)
	require.NoError(t, MapSQLError(nil))

	handle := newCGOSQLiteTestDB(
		t,
		filepath.Join(
			t.TempDir(),
			"empty",
		),
		false,
	)
	_, err := sqlc.NewSqlite(handle).GetChainInfo(t.Context(), "missing")
	require.Error(t, err)
	require.IsType(t, &ErrSchemaError{}, MapSQLError(err))
}

// TestCGOSQLiteUnknownPragma fails closed if a new connection setting has not
// been adapted to the CGO driver's DSN vocabulary.
func TestCGOSQLiteUnknownPragma(t *testing.T) {
	t.Parallel()

	_, err := OpenSQLiteDatabase(SQLiteOpenConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "unused.db"),
		Pragmas: []SQLitePragma{
			{Name: "unsupported", Value: "on"},
		},
	})
	require.ErrorContains(t, err, "unsupported CGO SQLite pragma")
}

// TestCGOSQLiteInvalidFullfsync refuses a malformed durability setting before
// opening the file, rather than silently using SQLite's default.
func TestCGOSQLiteInvalidFullfsync(t *testing.T) {
	t.Parallel()

	result, err := OpenSQLiteDatabase(SQLiteOpenConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "unused.db"),
		Pragmas: []SQLitePragma{
			{Name: "fullfsync", Value: "invalid"},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.DB.Close()) })
	require.ErrorContains(t, result.DB.Ping(), "invalid SQLite fullfsync")
}
