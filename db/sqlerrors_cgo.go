//go:build (!js || !wasm) && sqlite_cgo

package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// mapSQLiteError preserves transaction retry and constraint handling when
// Android uses the CGO driver, whose errors are values with extended codes.
func mapSQLiteError(err error) error {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return nil
	}

	// Check extended codes first: BUSY_SNAPSHOT requires restarting the
	// transaction, whereas ordinary BUSY can wait for another writer.
	switch sqliteErr.ExtendedCode {
	case sqlite3.ErrConstraintUnique, sqlite3.ErrConstraintPrimaryKey:
		return &ErrSQLUniqueConstraintViolation{DBError: sqliteErr}

	case sqlite3.ErrBusySnapshot:
		return &ErrDeadlockError{DBError: sqliteErr}
	}

	switch sqliteErr.Code {
	case sqlite3.ErrBusy:
		return &ErrSerializationError{DBError: sqliteErr}

	case sqlite3.ErrLocked:
		return &ErrDeadlockError{DBError: sqliteErr}

	case sqlite3.ErrError:
		if strings.Contains(sqliteErr.Error(), "no such table") {
			return &ErrSchemaError{DBError: sqliteErr}
		}
	}

	return fmt.Errorf("unknown sqlite error: %w", sqliteErr)
}
