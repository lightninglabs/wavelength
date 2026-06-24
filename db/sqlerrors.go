package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	pgconnv4 "github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	pgconnv5 "github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// isDBClosedError reports whether err indicates the underlying sql handle
// has already been closed. Both sqlite and postgres surface a few different
// shapes for this depending on whether the close races against a conn-pool
// borrow, an in-flight tx begin, or a new ExecTx call. Used by ExecTx to
// demote the warning fired during teardown — at that point every actor's
// in-flight DB call is expected to fail.
func isDBClosedError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}

	msg := err.Error()
	closedHints := []string{
		"sql: database is closed",
		"database is closed",
		"use of closed network connection",
	}
	for _, h := range closedHints {
		if strings.Contains(msg, h) {
			return true
		}
	}

	return false
}

var (
	// ErrRetriesExceeded is returned when a transaction is retried more
	// than the max allowed valued without a success.
	ErrRetriesExceeded = errors.New("db tx retries exceeded")
)

// MapSQLError attempts to interpret a given error as a database agnostic SQL
// error.
func MapSQLError(err error) error {
	// Attempt to interpret the error as a sqlite error.
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return parseSqliteError(sqliteErr)
	}

	// Attempt to interpret the error as a postgres error. The pgx v4 and
	// v5 stdlib drivers return distinct *pgconn.PgError types, so match
	// both and classify by the shared SQLSTATE code.
	var pgErrV4 *pgconnv4.PgError
	if errors.As(err, &pgErrV4) {
		return classifyPostgresError(pgErrV4.Code, pgErrV4)
	}

	var pgErrV5 *pgconnv5.PgError
	if errors.As(err, &pgErrV5) {
		return classifyPostgresError(pgErrV5.Code, pgErrV5)
	}

	// As a last step, check if this is a connection error that needs
	// sanitization to prevent leaking sensitive information.
	err = sanitizeConnectionError(err)

	// Return the error (potentially sanitized) if it could not be
	// classified as a database specific error.
	return err
}

// parseSqliteError attempts to parse a sqlite error as a database agnostic
// SQL error.
func parseSqliteError(sqliteErr *sqlite.Error) error {
	switch sqliteErr.Code() {
	// Handle unique constraint violation error.
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE,
		sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return &ErrSQLUniqueConstraintViolation{
			DBError: sqliteErr,
		}

	// Database is currently busy, so we'll need to try again.
	case sqlite3.SQLITE_BUSY:
		return &ErrSerializationError{
			DBError: sqliteErr,
		}

	// A write operation could not continue because of a conflict within
	// the same database connection.
	case sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_BUSY_SNAPSHOT:
		return &ErrDeadlockError{
			DBError: sqliteErr,
		}

	// Generic error, need to parse the message further.
	case sqlite3.SQLITE_ERROR:
		errMsg := sqliteErr.Error()

		switch {
		case strings.Contains(errMsg, "no such table"):
			return &ErrSchemaError{
				DBError: sqliteErr,
			}

		default:
			return fmt.Errorf("unknown sqlite error: %w", sqliteErr)
		}

	default:
		return fmt.Errorf("unknown sqlite error: %w", sqliteErr)
	}
}

// classifyPostgresError maps a postgres SQLSTATE code to a database agnostic
// SQL error.
func classifyPostgresError(code string, dbErr error) error {
	switch code {
	// Handle unique constraint violation error.
	case pgerrcode.UniqueViolation:
		return &ErrSQLUniqueConstraintViolation{
			DBError: dbErr,
		}

	// Unable to serialize the transaction, so we'll need to try again.
	case pgerrcode.SerializationFailure:
		return &ErrSerializationError{
			DBError: dbErr,
		}

	// A write operation could not continue because of a conflict within
	// the same database connection.
	case pgerrcode.DeadlockDetected:
		return &ErrDeadlockError{
			DBError: dbErr,
		}

	// Handle schema error.
	case pgerrcode.UndefinedColumn, pgerrcode.UndefinedTable:
		return &ErrSchemaError{
			DBError: dbErr,
		}

	default:
		return fmt.Errorf("unknown postgres error: %w",
			sanitizeConnectionError(dbErr))
	}
}

// ErrSQLUniqueConstraintViolation is an error type which represents a database
// agnostic SQL unique constraint violation.
type ErrSQLUniqueConstraintViolation struct {
	DBError error
}

func (e ErrSQLUniqueConstraintViolation) Error() string {
	return fmt.Sprintf("sql unique constraint violation: %v", e.DBError)
}

// ErrSerializationError is an error type which represents a database agnostic
// error that a transaction couldn't be serialized with other concurrent db
// transactions.
type ErrSerializationError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrSerializationError) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrSerializationError) Error() string {
	return e.DBError.Error()
}

// ErrDeadlockError is an error type which represents a database agnostic error
// where transactions have led to cyclic dependencies in lock acquisition.
type ErrDeadlockError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrDeadlockError) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrDeadlockError) Error() string {
	return e.DBError.Error()
}

// IsSerializationError returns true if the given error is a serialization
// error.
func IsSerializationError(err error) bool {
	var serializationError *ErrSerializationError

	return errors.As(err, &serializationError)
}

// IsDeadlockError returns true if the given error is a deadlock error.
func IsDeadlockError(err error) bool {
	var deadlockError *ErrDeadlockError

	return errors.As(err, &deadlockError)
}

// IsSerializationOrDeadlockError returns true if the given error is either a
// deadlock error or a serialization error.
func IsSerializationOrDeadlockError(err error) bool {
	return IsDeadlockError(err) || IsSerializationError(err)
}

// ErrSchemaError is an error type which represents a database agnostic error
// that the schema of the database is incorrect for the given query.
type ErrSchemaError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrSchemaError) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrSchemaError) Error() string {
	return e.DBError.Error()
}

// IsSchemaError returns true if the given error is a schema error.
func IsSchemaError(err error) bool {
	var schemaError *ErrSchemaError

	return errors.As(err, &schemaError)
}

// ErrDatabaseConnectionError is an error type which represents a database
// connection error with sensitive information sanitized.
type ErrDatabaseConnectionError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrDatabaseConnectionError) Unwrap() error {
	return e.DBError
}

// Error returns a generic error message without revealing connection details.
func (e ErrDatabaseConnectionError) Error() string {

	// Return a generic error message that doesn't reveal any connection
	// details to prevent information leakage.
	return "database connection failed"
}

// isConnectionError checks if an error message contains patterns that indicate
// a database connection error with potentially sensitive information.
func isConnectionError(errStr string) bool {
	// List of patterns that indicate connection errors with sensitive info.
	patterns := []string{
		"failed to connect to",
		"dial tcp",
		"user=",
		"password=",
		"host=",
		"dbname=",
		"sslmode=",
		"connection refused",
		"no route to host",
		"password authentication failed",
	}

	for _, pattern := range patterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// sanitizeConnectionError checks if an error contains database connection
// information and returns a sanitized version if it does.
func sanitizeConnectionError(err error) error {
	if err == nil {
		return nil
	}

	// Check if the error message contains connection parameters that could
	// leak sensitive information.
	if isConnectionError(err.Error()) {

		// Return a sanitized version to prevent information
		// leakage. The original error is stored in the DBError
		// field for debugging purposes when needed.
		return &ErrDatabaseConnectionError{
			DBError: err,
		}
	}

	return err
}
