//go:build js && wasm

package db

import "strings"

// mapSQLiteError classifies browser SQLite errors using stable message
// fragments surfaced through the wasmsqlite bridge.
func mapSQLiteError(err error) error {
	if err == nil {
		return nil
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unique constraint failed"),
		strings.Contains(msg, "constraint failed"),
		strings.Contains(msg, "primary key"):
		return &ErrSQLUniqueConstraintViolation{
			DBError: err,
		}

	case strings.Contains(msg, "database is locked"),
		strings.Contains(msg, "database table is locked"):
		return &ErrDeadlockError{
			DBError: err,
		}

	case strings.Contains(msg, "database is busy"):
		return &ErrSerializationError{
			DBError: err,
		}

	case strings.Contains(msg, "no such table"):
		return &ErrSchemaError{
			DBError: err,
		}

	default:
		return nil
	}
}
