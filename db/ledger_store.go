package db

import (
	"context"
	"database/sql"

	"github.com/lightninglabs/darepo-client/db/sqlc"
)

// LedgerEntry is a client-side double-entry ledger record. When the
// ledgeractor package lands, this type will be replaced by the domain
// type defined there.
type LedgerEntry struct {
	DebitAccount  string
	CreditAccount string
	AmountSat     int64
	RoundID       []byte
	EventType     string
	Description   string
	CreatedAt     int64
}

// LedgerStore is the persistence interface for the client-side fee
// ledger. The ledgeractor package will define the canonical version of
// this interface; this local copy allows the DB layer to compile and
// be tested independently.
type LedgerStore interface {
	// InsertLedgerEntry persists a single double-entry record.
	InsertLedgerEntry(ctx context.Context, entry LedgerEntry) error

	// GetAccountBalance returns the net balance (debits minus
	// credits) for the given account.
	GetAccountBalance(ctx context.Context, accountID string) (
		int64, error)

	// GetTotalOperatorFeesPaid returns the cumulative satoshis
	// debited to the fees_paid expense account.
	GetTotalOperatorFeesPaid(ctx context.Context) (int64, error)

	// ListLedgerEntries returns a paginated list of entries ordered
	// by creation time descending.
	ListLedgerEntries(ctx context.Context, limit,
		offset int32) ([]sqlc.LedgerEntry, error)

	// ListLedgerEntriesByType returns a paginated list of entries
	// filtered by event type.
	ListLedgerEntriesByType(ctx context.Context, eventType string,
		limit, offset int32) ([]sqlc.LedgerEntry, error)

	// CountLedgerEntries returns the total number of entries.
	CountLedgerEntries(ctx context.Context) (int64, error)

	// ListAccounts returns all accounts in the chart of accounts.
	ListAccounts(ctx context.Context) ([]sqlc.Account, error)
}

// Compile-time check that LedgerStoreDB implements LedgerStore.
var _ LedgerStore = (*LedgerStoreDB)(nil)

// LedgerStoreDB bridges the client-side LedgerStore interface to the
// sqlc-generated queries. This adapter converts LedgerEntry to
// sqlc.InsertClientLedgerEntryParams and wraps all operations in
// ExecTx for transactional safety.
type LedgerStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewLedgerStoreDB creates a new LedgerStoreDB from a Store.
func NewLedgerStoreDB(store *Store) *LedgerStoreDB {
	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &LedgerStoreDB{
		TransactionExecutor: txExec,
	}
}

// InsertLedgerEntry persists a client-side double-entry ledger record
// within a database transaction.
func (s *LedgerStoreDB) InsertLedgerEntry(ctx context.Context,
	entry LedgerEntry) error {

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertClientLedgerEntry(
				ctx, sqlc.InsertClientLedgerEntryParams{
					DebitAccount:  entry.DebitAccount,
					CreditAccount: entry.CreditAccount,
					AmountSat:     entry.AmountSat,
					RoundID:       entry.RoundID,
					EventType:     entry.EventType,
					Description:   entry.Description,
					CreatedAt:     entry.CreatedAt,
				},
			)
		},
	)
}

// GetAccountBalance returns the net balance (debits minus credits) for
// the given account within a read transaction.
func (s *LedgerStoreDB) GetAccountBalance(ctx context.Context,
	accountID string) (int64, error) {

	var balance int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			balance, txErr = qtx.GetClientAccountBalance(
				ctx, accountID,
			)

			return txErr
		},
	)

	return balance, err
}

// GetTotalOperatorFeesPaid returns the cumulative satoshis debited to
// the fees_paid expense account within a read transaction.
func (s *LedgerStoreDB) GetTotalOperatorFeesPaid(
	ctx context.Context) (int64, error) {

	var total int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			total, txErr = qtx.GetTotalOperatorFeesPaid(ctx)
			return txErr
		},
	)

	return total, err
}

// ListLedgerEntries returns a paginated list of ledger entries ordered
// by creation time within a read transaction.
func (s *LedgerStoreDB) ListLedgerEntries(ctx context.Context,
	limit, offset int32) ([]sqlc.LedgerEntry, error) {

	var entries []sqlc.LedgerEntry
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListClientLedgerEntries(
				ctx, sqlc.ListClientLedgerEntriesParams{
					Limit:  limit,
					Offset: offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// ListLedgerEntriesByType returns a paginated list of ledger entries
// filtered by event type within a read transaction.
func (s *LedgerStoreDB) ListLedgerEntriesByType(ctx context.Context,
	eventType string, limit, offset int32) ([]sqlc.LedgerEntry, error) {

	var entries []sqlc.LedgerEntry
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListClientLedgerEntriesByType(
				ctx,
				sqlc.ListClientLedgerEntriesByTypeParams{
					EventType: eventType,
					Limit:     limit,
					Offset:    offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// CountLedgerEntries returns the total number of ledger entries within
// a read transaction.
func (s *LedgerStoreDB) CountLedgerEntries(
	ctx context.Context) (int64, error) {

	var count int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			count, txErr = qtx.CountClientLedgerEntries(ctx)
			return txErr
		},
	)

	return count, err
}

// ListAccounts returns all accounts in the chart of accounts within a
// read transaction.
func (s *LedgerStoreDB) ListAccounts(
	ctx context.Context) ([]sqlc.Account, error) {

	var accounts []sqlc.Account
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			accounts, txErr = qtx.ListClientAccounts(ctx)
			return txErr
		},
	)

	return accounts, err
}
