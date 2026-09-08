package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
	prand "math/rand"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

var (
	// DefaultStoreTimeout is the default timeout used for any interaction
	// with the storage/database.
	DefaultStoreTimeout = time.Second * 10
)

const (
	// DefaultNumTxRetries is the default number of times we'll retry a
	// transaction if it fails with an error that permits transaction
	// repetition.
	DefaultNumTxRetries = 10

	// DefaultInitialRetryDelay is the default initial delay between
	// retries. This will be used to generate a random delay between -50%
	// and +50% of this value, so 20 to 60 milliseconds. The retry will be
	// doubled after each attempt until we reach DefaultMaxRetryDelay. We
	// start with a random value to avoid multiple goroutines that are
	// created at the same time to effectively retry at the same time.
	DefaultInitialRetryDelay = time.Millisecond * 40

	// DefaultMaxRetryDelay is the default maximum delay between retries.
	DefaultMaxRetryDelay = time.Second * 3
)

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can either be read or write.
type TxOptions interface {
	// ReadOnly returns true if the transaction should be read-only.
	ReadOnly() bool
}

// BaseTxOptions defines the set of db txn options the database understands.
type BaseTxOptions struct {
	// readOnly governs if a read-only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions interface.
func (a *BaseTxOptions) ReadOnly() bool {
	return a.readOnly
}

// ReadTxOption returns a TxOptions that indicates a read-only transaction.
func ReadTxOption() *BaseTxOptions {
	return &BaseTxOptions{
		readOnly: true,
	}
}

// WriteTxOption returns a TxOptions that indicates a write transaction.
func WriteTxOption() *BaseTxOptions {
	return &BaseTxOptions{
		readOnly: false,
	}
}

// BatchedTx is a generic interface that represents the ability to execute
// several operations to a given storage interface in a single atomic
// transaction. Typically, Q here will be some subset of the main sqlc.Querier
// interface allowing it to only depend on the routines it needs to implement
// any additional business logic.
type BatchedTx[Q any] interface {
	// ExecTx will execute the passed txBody, operating upon generic
	// parameter Q (usually a storage interface) in a single transaction.
	// The set of TxOptions are passed in order to allow the caller to
	// specify if a transaction should be read-only and optionally what
	// type of concurrency control should be used.
	ExecTx(ctx context.Context, txOptions TxOptions,
		txBody func(Q) error) error

	// Backend returns the type of the database backend used.
	Backend() sqlc.BackendType
}

// Tx represents a database transaction that can be committed or rolled back.
type Tx interface {
	// Commit commits the database transaction, an error should be returned
	// if the commit isn't possible.
	Commit() error

	// Rollback rolls back an incomplete database transaction.
	// Transactions that were able to be committed can still call this as a
	// noop.
	Rollback() error
}

// QueryCreator is a generic function that's used to create a Querier, which is
// a type of interface that implements storage related methods from a database
// transaction. This will be used to instantiate an object callers can use to
// apply multiple modifications to an object interface in a single atomic
// transaction.
type QueryCreator[Q any] func(*sql.Tx) Q

// BatchedQuerier is a generic interface that allows callers to create a new
// database transaction based on an abstract type that implements the TxOptions
// interface.
type BatchedQuerier interface {
	// Querier is the underlying query source, this is in place so we can
	// pass a BatchedQuerier implementation directly into objects that
	// create a batched version of the normal methods they need.
	sqlc.Querier

	// BeginTx creates a new database transaction given the set of
	// transaction options.
	BeginTx(ctx context.Context, options TxOptions) (*sql.Tx, error)

	// Backend returns the type of the database backend used.
	Backend() sqlc.BackendType
}

// txExecutorOptions is a struct that holds the options for the transaction
// executor. This can be used to do things like retry a transaction due to an
// error a certain amount of times.
type txExecutorOptions struct {
	numRetries        int
	initialRetryDelay time.Duration
	maxRetryDelay     time.Duration
}

// defaultTxExecutorOptions returns the default options for the transaction
// executor.
func defaultTxExecutorOptions() *txExecutorOptions {
	return &txExecutorOptions{
		numRetries:        DefaultNumTxRetries,
		initialRetryDelay: DefaultInitialRetryDelay,
		maxRetryDelay:     DefaultMaxRetryDelay,
	}
}

// randRetryDelay returns a random retry delay between -50% and +50% of the
// configured delay that is doubled for each attempt and capped at a max value.
func (t *txExecutorOptions) randRetryDelay(attempt int) time.Duration {
	halfDelay := t.initialRetryDelay / 2
	randDelay := prand.Int63n(int64(t.initialRetryDelay)) //nolint:gosec

	// 50% plus 0%-100% gives us the range of 50%-150%.
	initialDelay := halfDelay + time.Duration(randDelay)

	// If this is the first attempt, we just return the initial delay.
	if attempt == 0 {
		return initialDelay
	}

	// For each subsequent delay, we double the initial delay. This still
	// gives us a somewhat random delay, but it still increases with each
	// attempt. If we double something n times, that's the same as
	// multiplying the value with 2^n. We limit the power to 32 to avoid
	// overflows.
	factor := time.Duration(math.Pow(2, math.Min(float64(attempt), 32)))
	//nolint:durationcheck
	actualDelay := initialDelay * factor

	// Cap the delay at the maximum configured value.
	if actualDelay > t.maxRetryDelay {
		return t.maxRetryDelay
	}

	return actualDelay
}

// RandRetryDelay returns the default randomized backoff for the given retry
// attempt, doubling with each attempt and capped at DefaultMaxRetryDelay.
//
// This exists so that transaction executors outside this type, such as the
// durable actor framework's commit transaction, can back off on exactly the
// same schedule instead of growing a second copy of the policy.
func RandRetryDelay(attempt int) time.Duration {
	return defaultTxExecutorOptions().randRetryDelay(attempt)
}

// TxExecutorOption is a functional option that allows us to pass in optional
// argument when creating the executor.
type TxExecutorOption func(*txExecutorOptions)

// WithTxRetries is a functional option that allows us to specify the number of
// times a transaction should be retried if it fails with a repeatable error.
func WithTxRetries(numRetries int) TxExecutorOption {
	return func(o *txExecutorOptions) {
		o.numRetries = numRetries
	}
}

// WithTxRetryDelay is a functional option that allows us to specify the delay
// to wait before a transaction is retried.
func WithTxRetryDelay(delay time.Duration) TxExecutorOption {
	return func(o *txExecutorOptions) {
		o.initialRetryDelay = delay
	}
}

// TransactionExecutor is a generic struct that abstracts away from the type of
// query a type needs to run under a database transaction, and also the set of
// options for that transaction. The QueryCreator is used to create a query
// given a database transaction created by the BatchedQuerier.
type TransactionExecutor[Query any] struct {
	BatchedQuerier

	createQuery QueryCreator[Query]

	opts *txExecutorOptions

	log btclog.Logger
}

// NewTransactionExecutor creates a new instance of a TransactionExecutor given
// a Querier query object and a concrete type for the type of transactions the
// Querier understands.
func NewTransactionExecutor[Querier any](db BatchedQuerier,
	createQuery QueryCreator[Querier], log btclog.Logger,
	opts ...TxExecutorOption) *TransactionExecutor[Querier] {

	txOpts := defaultTxExecutorOptions()
	for _, optFunc := range opts {
		optFunc(txOpts)
	}

	return &TransactionExecutor[Querier]{
		BatchedQuerier: db,
		createQuery:    createQuery,
		opts:           txOpts,
		log:            log,
	}
}

// isExpectedTxShutdownErr reports transaction errors caused by explicit
// cancellation or database teardown. It inspects the transaction error rather
// than the context so an unrelated failure that coincides with cancellation
// remains visible. A deadline is not shutdown: it can indicate a slow backend
// and must remain visible at warning level.
func isExpectedTxShutdownErr(dbErr error) bool {
	return errors.Is(dbErr, context.Canceled) || isDBClosedError(dbErr)
}

// ExecTx is a wrapper for txBody to abstract the creation and commit of a db
// transaction. The db transaction is embedded in a `*Queries` that txBody
// needs to use when executing each one of the queries that need to be applied
// atomically. This can be used by other storage interfaces to parameterize the
// type of query and options run, in order to have access to batched operations
// related to a storage object.
func (t *TransactionExecutor[Q]) ExecTx(ctx context.Context,
	txOptions TxOptions, txBody func(Q) error) error {

	// If the context already carries a database transaction from the
	// durable actor framework, join it instead of creating a new one.
	// This ensures that all store operations within a single actor
	// message are executed atomically in the same transaction. The
	// provided txOptions are ignored in this case; isolation level
	// and read-only semantics are governed by the outer transaction.
	if tx, ok := actor.TxFromContext(ctx); ok {
		return txBody(t.createQuery(tx))
	}

	waitBeforeRetry := func(attemptNumber int) {
		retryDelay := t.opts.randRetryDelay(attemptNumber)

		t.log.TraceS(
			ctx, "Retrying transaction due to tx serialization "+
				"or deadlock error", "attempt_number",
			attemptNumber, "delay", retryDelay,
		)

		// Before we try again, we'll wait with a random backoff based
		// on the retry delay.
		time.Sleep(retryDelay)
	}

	for i := 0; i < t.opts.numRetries; i++ {
		// Create the db transaction.
		tx, err := t.BatchedQuerier.BeginTx(ctx, txOptions)
		if err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationOrDeadlockError(dbErr) {
				// Nothing to roll back here, since we didn't
				// even get a transaction yet.
				waitBeforeRetry(i)
				continue
			}

			// During shutdown the underlying *sql.DB is closed
			// before every actor's lease/poll loop has wound down,
			// which produces a flood of "sql: database is closed"
			// warnings at the tail of every itest. Treat that case
			// as expected and demote to debug; real production
			// failures (corrupt DB, disk full, …) still surface as
			// WARN because their error is not caused by
			// cancellation or teardown.
			if isExpectedTxShutdownErr(dbErr) {
				t.log.DebugS(ctx, "Transaction begin failed "+
					"during shutdown", "err", dbErr)

				return dbErr
			}

			t.log.WarnS(ctx, "Transaction begin failed", dbErr)

			return dbErr
		}

		// Rollback is safe to call even if the tx is already closed,
		// so if the tx commits successfully, this is a no-op.
		defer func() {
			_ = tx.Rollback()
		}()

		if err := txBody(t.createQuery(tx)); err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationOrDeadlockError(dbErr) {
				// Roll back the transaction, then pop back up
				// to try once again.
				_ = tx.Rollback()

				waitBeforeRetry(i)

				continue
			}

			// Explicit cancellation interrupts the body before
			// commit. The transaction rolls back and the caller
			// owns recovery, so the failure is lifecycle noise
			// rather than an operational warning.
			if isExpectedTxShutdownErr(dbErr) {
				t.log.DebugS(ctx, "Transaction body failed "+
					"during shutdown", "err", dbErr)

				return dbErr
			}

			// A missing row is a normal negative lookup for several
			// stores, for example when wallet UTXOs are checked
			// against known boarding addresses. Let callers decide
			// whether that miss is noteworthy instead of warning
			// at the transaction layer.
			if !errors.Is(dbErr, sql.ErrNoRows) {
				t.log.WarnS(ctx, "Transaction body failed",
					dbErr,
				)
			}

			return dbErr
		}

		// Commit transaction.
		if err = tx.Commit(); err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationOrDeadlockError(dbErr) {
				// Roll back the transaction, then pop back up
				// to try once again.
				_ = tx.Rollback()

				waitBeforeRetry(i)

				continue
			}

			if isExpectedTxShutdownErr(dbErr) {
				// Unlike begin or body cancellation, reaching
				// commit means the body completed work that is
				// now discarded. Keep that event greppable
				// without alerting during expected shutdown.
				t.log.InfoS(ctx, "Transaction commit failed "+
					"during shutdown", "err", dbErr)

				return dbErr
			}

			t.log.WarnS(ctx, "Transaction commit failed", dbErr)

			return dbErr
		}

		return nil
	}

	// If we get to this point, then we weren't able to successfully commit
	// a tx given the max number of retries.
	t.log.WarnS(ctx, "Transaction retries exhausted", ErrRetriesExceeded)

	return ErrRetriesExceeded
}

// Backend returns the type of the database backend used.
func (t *TransactionExecutor[Q]) Backend() sqlc.BackendType {
	return t.BatchedQuerier.Backend()
}

// BaseDB is the base database struct that each implementation can embed to
// gain some common functionality.
type BaseDB struct {
	*sql.DB

	*sqlc.Queries
}

// txIsolationLevel returns the isolation level a transaction against the given
// backend should be opened with.
//
// Read-write transactions always run at SERIALIZABLE, which is the level the
// storage layer has always promised writers. Read-only transactions on Postgres
// are instead opened at REPEATABLE READ, which in Postgres is snapshot
// isolation: the transaction reads from a single consistent snapshot for its
// whole lifetime, taken when its first statement runs rather than at BEGIN.
//
// That is a real, if modest, weakening. Snapshot isolation is not
// serializability, so the reader is no longer guaranteed to observe a state
// that corresponds to some serial ordering of the writers running alongside it.
// The read-only transaction anomaly described by Fekete and O'Neil is once
// again permitted. We accept that because our read paths only ever consume a
// point-in-time view of the database and never depended on being ordered
// against writers in other transactions. A read that feeds a later write in a
// separate transaction was never protected across that boundary at any
// isolation level.
//
// In exchange, a read-only REPEATABLE READ transaction takes no part in
// Postgres' serializable snapshot isolation conflict graph. It acquires no
// SIRead predicate locks, is not itself subject to SSI serialization failures,
// and can no longer cause a concurrent writer to be aborted as a pivot. Since
// the daemon is extremely read heavy, this removes a large amount of needless
// abort pressure from the system.
//
// SQLite is always effectively serializable because it only ever admits a
// single writer, so there is nothing to gain there and we leave it alone.
func txIsolationLevel(backend sqlc.BackendType,
	readOnly bool) sql.IsolationLevel {

	if readOnly && backend == sqlc.BackendTypePostgres {
		return sql.LevelRepeatableRead
	}

	return sql.LevelSerializable
}

// BeginTx wraps the normal sql specific BeginTx method with the TxOptions
// interface. This interface is then mapped to the concrete sql tx options
// struct.
func (s *BaseDB) BeginTx(ctx context.Context, opts TxOptions) (*sql.Tx, error) {
	readOnly := opts.ReadOnly()

	// The read-only flag is not just advisory here: Postgres only skips
	// predicate lock acquisition for a transaction that is actually
	// declared READ ONLY, so the flag is what makes the relaxed isolation
	// level worth anything.
	sqlOptions := sql.TxOptions{
		ReadOnly:  readOnly,
		Isolation: txIsolationLevel(s.Backend(), readOnly),
	}

	return s.DB.BeginTx(ctx, &sqlOptions)
}

// Backend returns the type of the database backend used.
func (s *BaseDB) Backend() sqlc.BackendType {
	return s.Queries.Backend()
}
