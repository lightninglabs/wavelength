//go:build !js || !wasm

package db

import (
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// mapSQLiteError attempts to parse native SQLite errors as database agnostic
// SQL errors.
func mapSQLiteError(err error) error {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return nil
	}

	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE,
		sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return &ErrSQLUniqueConstraintViolation{
			DBError: sqliteErr,
		}

	case sqlite3.SQLITE_BUSY:
		return &ErrSerializationError{
			DBError: sqliteErr,
		}

	case sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_BUSY_SNAPSHOT:
		return &ErrDeadlockError{
			DBError: sqliteErr,
		}

	case sqlite3.SQLITE_ERROR:
		errMsg := sqliteErr.Error()
		if strings.Contains(errMsg, "no such table") {
			return &ErrSchemaError{
				DBError: sqliteErr,
			}
		}

		return fmt.Errorf("unknown sqlite error: %w", sqliteErr)

	default:
		return fmt.Errorf("unknown sqlite error: %w", sqliteErr)
	}
}
