package actor

import (
	"context"
	"database/sql"
	"fmt"
)

// ErrNoTransactionInContext indicates that a transaction was expected in the
// context but none was found.
var ErrNoTransactionInContext = fmt.Errorf("no transaction in context")

// txContextKey is the context key for database transactions.
type txContextKey struct{}

// WithTx returns a new context with the given database transaction attached.
// This enables passing transactions through the call chain without modifying
// function signatures. Used primarily for:
//   - mailbox.Send() to write outbox messages in the same transaction as FSM state
//   - Environment storage operations to participate in actor transactions
//
// The transaction should only be used within the lifetime of the ExecTx closure
// that created it.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext retrieves the database transaction from the context, if present.
// Returns the transaction and true if found, nil and false otherwise.
//
// Callers should check the boolean return value before using the transaction:
//
//	if tx, ok := TxFromContext(ctx); ok {
//	    // Use tx for database operations
//	} else {
//	    // Fall back to non-transactional operation
//	}
func TxFromContext(ctx context.Context) (*sql.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(*sql.Tx)
	return tx, ok
}

// RequireTx extracts a transaction from the context or returns an error.
// Use this when a transaction is required and the absence should be an error.
func RequireTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return nil, ErrNoTransactionInContext
	}

	return tx, nil
}

// WithoutTx returns a new context with the database transaction stripped.
// This is used at actor boundaries (e.g., DurableMailbox.Send) to prevent
// the sender's transaction from leaking into the receiver's mailbox store.
// An untyped nil shadows the parent's txContextKey entry so that
// TxFromContext returns (nil, false).
func WithoutTx(ctx context.Context) context.Context {
	return context.WithValue(ctx, txContextKey{}, nil)
}

// HasTx returns true if the context contains a database transaction.
func HasTx(ctx context.Context) bool {
	_, ok := TxFromContext(ctx)
	return ok
}

// TxQuerier is a minimal interface for database operations that can be executed
// either directly or within a transaction. This allows code to work with both
// *sql.DB and *sql.Tx transparently.
type TxQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Ensure *sql.Tx implements TxQuerier.
var _ TxQuerier = (*sql.Tx)(nil)

// outboxIDContextKey is the context key for propagating the outbox message ID
// to the target actor's mailbox during CDC delivery. When the OutboxPublisher
// delivers a message, it injects the outbox row ID into the context so the
// receiving DurableMailbox uses it as the inbox message ID instead of generating
// a fresh one. This gives us receiver-side deduplication for free: if
// CompleteOutbox fails after a successful Tell, the retry will attempt to
// INSERT the same ID. The ON CONFLICT (id) DO NOTHING clause on
// EnqueueMailboxMessage makes this a silent no-op.
type outboxIDContextKey struct{}

// WithOutboxID returns a new context carrying the outbox message ID. The
// OutboxPublisher calls this before Tell so the downstream mailbox can reuse
// the ID for idempotent enqueue.
func WithOutboxID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, outboxIDContextKey{}, id)
}

// OutboxIDFromContext retrieves the outbox message ID from the context, if
// present. Returns the ID and true if found, empty string and false otherwise.
func OutboxIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(outboxIDContextKey{}).(string)
	return id, ok && id != ""
}
