//go:build (!js || !wasm) && sqlite_cgo

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/mattn/go-sqlite3"
)

const cgoSQLiteDriverName = "wavelength-sqlite3"

// enableSQLiteFullfsync is connection setup rather than a schema migration.
// sqlc cannot parse SQLite PRAGMAs, so keep this bootstrap SQL in a separate
// embedded file instead of mixing it into the generated application queries.
//
//go:embed sqlite_fullfsync.sql
var enableSQLiteFullfsync string

// sqliteTokens protects literals, quoted identifiers, comments, and ordinary
// identifiers from parameter rewriting. SQLite also permits Tcl-style named
// parameters, such as $name::suffix(value), which must remain intact.
var sqliteTokens = regexp.MustCompile(
	`--[^\r\n]*|/\*[\s\S]*?(?:\*/|$)|` +
		`'(?:[^']|'')*(?:'|$)|"(?:[^"]|"")*(?:"|$)|` +
		"`(?:[^`]|``)*(?:`|$)|" + `\[[^\]]*(?:\]|$)|` +
		`[$:@][\pL\pN_$]+(?:::[\pL\pN_$]+)*(?:\([^)]*\))?|` +
		`[\pL\pN_][\pL\pN_$]*`,
)

// init registers the adapter separately from mattn's stock driver so every
// connection, including those opened later by database/sql, preserves sqlc's
// numbered parameter semantics.
func init() {
	sql.Register(cgoSQLiteDriverName, &cgoSQLiteDriver{})
}

// cgoSQLiteDriver adapts PostgreSQL-style numbered sqlc placeholders to
// SQLite's numbered placeholders without changing the queries or arguments.
type cgoSQLiteDriver struct {
	sqlite3.SQLiteDriver
}

// Open wraps each connection while retaining the underlying driver's DSN
// setup, transactions, cancellation, and connection lifecycle.
func (d *cgoSQLiteDriver) Open(name string) (driver.Conn, error) {
	// mattn has no DSN option for fullfsync. Parse our extension before
	// opening the connection and apply it to every new handle, so pooled
	// replacements preserve the configured Darwin durability barrier.
	var fullfsync bool
	if pos := strings.IndexByte(name, '?'); pos >= 0 {
		options, err := url.ParseQuery(name[pos+1:])
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(options.Get("_fullfsync")) {
		case "1", "true", "on", "yes":
			fullfsync = true

		case "", "0", "false", "off", "no":
			// New SQLite connections default to fullfsync off.

		default:
			return nil, fmt.Errorf("invalid SQLite fullfsync: %q",
				options.Get("_fullfsync"))
		}
	}

	conn, err := d.SQLiteDriver.Open(name)
	if err != nil {
		return nil, err
	}

	sqliteConn, ok := conn.(*sqlite3.SQLiteConn)
	if !ok {
		_ = conn.Close()

		return nil, fmt.Errorf("unexpected SQLite connection: %T", conn)
	}
	if fullfsync {
		_, err := sqliteConn.Exec(enableSQLiteFullfsync, nil)
		if err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("enable SQLite fullfsync: %w",
				err)
		}
	}

	return &cgoSQLiteConn{SQLiteConn: sqliteConn}, nil
}

// cgoSQLiteConn intercepts query preparation on the fast Exec/Query paths as
// well as explicitly prepared statements. Its embedded connection supplies
// the remaining database/sql interfaces, including BeginTx and Ping.
type cgoSQLiteConn struct {
	*sqlite3.SQLiteConn
}

// Prepare applies numbered binding to explicitly prepared statements.
func (c *cgoSQLiteConn) Prepare(query string) (driver.Stmt, error) {
	return c.SQLiteConn.Prepare(sqliteNumberedParams(query))
}

// PrepareContext preserves numbered binding on contextual preparation.
func (c *cgoSQLiteConn) PrepareContext(ctx context.Context, query string) (
	driver.Stmt, error) {

	return c.SQLiteConn.PrepareContext(ctx, sqliteNumberedParams(query))
}

// ExecContext preserves numbered binding on database/sql's fast exec path.
func (c *cgoSQLiteConn) ExecContext(ctx context.Context, query string,
	args []driver.NamedValue) (driver.Result, error) {

	return c.SQLiteConn.ExecContext(ctx, sqliteNumberedParams(query), args)
}

// QueryContext preserves numbered binding on database/sql's fast query path.
func (c *cgoSQLiteConn) QueryContext(ctx context.Context, query string,
	args []driver.NamedValue) (driver.Rows, error) {

	return c.SQLiteConn.QueryContext(ctx, sqliteNumberedParams(query), args)
}

// sqliteNumberedParams converts $N to ?N outside literals and comments.
// SQLite assigns $N slots by first appearance, while modernc treats N as the
// argument ordinal. Using ?N preserves the ordinal even for reordered or
// repeated parameters, and leaves existing ? and named parameters alone.
func sqliteNumberedParams(query string) string {
	if !strings.Contains(query, "$") {
		return query
	}

	return sqliteTokens.ReplaceAllStringFunc(
		query,
		func(token string) string {
			if token[0] != '$' || len(token) < 2 {
				return token
			}
			for _, char := range token[1:] {
				if char < '0' || char > '9' {
					return token
				}
			}

			return "?" + token[1:]
		},
	)
}
