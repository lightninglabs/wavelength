package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	pgconnv4 "github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	pgconnv5 "github.com/jackc/pgx/v5/pgconn"
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
	if mapped := mapSQLiteError(err); mapped != nil {
		return mapped
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

	// A statement was issued against a transaction that an earlier failure
	// already aborted. Postgres rejects every further command until the
	// block ends, so this code is never the root cause, only its echo. The
	// root cause is frequently a serialization failure that an intervening
	// caller logged and swallowed, which leaves the retry layer looking at
	// 25P02 instead of the 40001 underneath it. Rolling back and replaying
	// the transaction is the only way forward either way.
	case pgerrcode.InFailedSQLTransaction:
		return &ErrAbortedTransaction{
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

// PgErrorDetail extracts the Detail field of an underlying Postgres error, if
// there is one. The Error method of pgconn.PgError renders only the severity,
// the message and the SQLSTATE code, so the Detail is otherwise dropped on the
// floor before it ever reaches a log line.
//
// That detail is the only thing that tells two very different 40001 aborts
// apart. A true serializable snapshot isolation abort carries a "Reason code"
// naming the transaction's role in the conflict graph, whereas an ordinary
// write-write conflict on the same row carries none. Once read-only
// transactions stop taking predicate locks, this is the signal that says
// whether a given write path still depends on SSI or would be equally happy at
// REPEATABLE READ. For a 23505 the detail names the constraint and the
// conflicting key values, which is what identifies a lost creation race.
func PgErrorDetail(err error) string {
	var pgErrV4 *pgconnv4.PgError
	if errors.As(err, &pgErrV4) {
		return pgErrV4.Detail
	}

	var pgErrV5 *pgconnv5.PgError
	if errors.As(err, &pgErrV5) {
		return pgErrV5.Detail
	}

	return ""
}

// PgErrorConstraint extracts the name of the constraint that a Postgres error
// was raised against, if there is one. The schema carries six partial unique
// indexes, and without the constraint name a 23505 raised by any of them looks
// exactly like a 23505 raised by the table's primary key.
func PgErrorConstraint(err error) string {
	var pgErrV4 *pgconnv4.PgError
	if errors.As(err, &pgErrV4) {
		return pgErrV4.ConstraintName
	}

	var pgErrV5 *pgconnv5.PgError
	if errors.As(err, &pgErrV5) {
		return pgErrV5.ConstraintName
	}

	return ""
}

// withPgDetail renders a database error together with the Postgres constraint
// name and detail when they are available, and falls back to the plain error
// otherwise.
func withPgDetail(err error) string {
	constraint := PgErrorConstraint(err)
	detail := PgErrorDetail(err)

	switch {
	case constraint != "" && detail != "":
		return fmt.Sprintf("%v (constraint: %s, detail: %s)", err,
			constraint, detail)

	case constraint != "":
		return fmt.Sprintf("%v (constraint: %s)", err, constraint)

	case detail != "":
		return fmt.Sprintf("%v (detail: %s)", err, detail)

	default:
		return err.Error()
	}
}

// ErrSQLUniqueConstraintViolation is an error type which represents a database
// agnostic SQL unique constraint violation.
type ErrSQLUniqueConstraintViolation struct {
	DBError error
}

func (e ErrSQLUniqueConstraintViolation) Error() string {
	return fmt.Sprintf("sql unique constraint violation: %v",
		withPgDetail(e.DBError))
}

// Unwrap returns the wrapped error.
//
// Without this, the mapped error is a dead end for errors.As, and the
// PgErrorConstraint and PgErrorDetail extractors return empty for every caller
// that holds the mapped error rather than the raw driver one. That is the
// normal case, since ExecTx returns the mapped error, and identifying which of
// the partial unique indexes actually fired is the whole point of surfacing
// the constraint name.
func (e ErrSQLUniqueConstraintViolation) Unwrap() error {
	return e.DBError
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
	return withPgDetail(e.DBError)
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
	return withPgDetail(e.DBError)
}

// IsUniqueConstraintViolation returns true if the given error is a unique
// constraint violation.
//
// This is deliberately not part of IsSerializationOrDeadlockError. A unique
// violation is not safe to retry blindly, because a retry of a plain insert
// that lost a creation race just loses it again. Callers that can lose such a
// race need to either rephrase the insert as a no-op upsert or translate the
// violation into a domain level "already exists", which is why the classifier
// is exposed separately.
func IsUniqueConstraintViolation(err error) bool {
	var uniqueErr *ErrSQLUniqueConstraintViolation

	return errors.As(err, &uniqueErr)
}

// ErrAbortedTransaction is an error type which represents a database agnostic
// error that a statement was issued against a transaction that an earlier
// failure had already aborted.
type ErrAbortedTransaction struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrAbortedTransaction) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrAbortedTransaction) Error() string {
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

// IsAbortedTransactionError returns true if the given error reports that the
// transaction the statement ran in had already been aborted.
func IsAbortedTransactionError(err error) bool {
	var abortedTx *ErrAbortedTransaction

	return errors.As(err, &abortedTx)
}

// IsSerializationOrDeadlockError returns true if the given error is either a
// deadlock error or a serialization error.
func IsSerializationOrDeadlockError(err error) bool {
	return IsDeadlockError(err) || IsSerializationError(err)
}

// IsRetryableTxError returns true if the given error means the transaction it
// came from can be replayed from the top with a reasonable chance of a
// different outcome.
//
// This is a superset of IsSerializationOrDeadlockError: it also covers the
// aborted-transaction state, which is what a caller that logs and swallows a
// serialization failure leaves behind for the next statement to trip over.
// Retrying on that echo costs a bounded number of replays when the underlying
// failure turns out to be deterministic, and recovers the transaction when it
// does not.
func IsRetryableTxError(err error) bool {
	return IsSerializationOrDeadlockError(err) ||
		IsAbortedTransactionError(err)
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
