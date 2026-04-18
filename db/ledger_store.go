package db

import (
	"context"
	"database/sql"

	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/fees"
)

// LedgerEntry aliases the fees package's domain-level
// double-entry record. Aliasing (rather than duplicating) keeps
// the adapter's wire format in lockstep with the fee-model
// definition and gives us the compile-time LedgerStore
// assertion below.
type LedgerEntry = fees.LedgerEntry

// LedgerStoreDB bridges the domain-level ledger entry type to
// the sqlc-generated queries. This adapter converts LedgerEntry
// to sqlc.InsertLedgerEntryParams and wraps all writes in
// ExecTx for transactional safety.
type LedgerStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

// Compile-time assertion that the adapter satisfies the
// fees.LedgerStore interface consumed by the fee recording
// helpers. This ensures any future signature change in
// fees.LedgerStore surfaces here as a build error.
var _ fees.LedgerStore = (*LedgerStoreDB)(nil)

// NewLedgerStoreDB creates a new LedgerStoreDB from a Store.
func NewLedgerStoreDB(store *Store) *LedgerStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &LedgerStoreDB{
		TransactionExecutor: txExec,
	}
}

// InsertLedgerEntry persists a double-entry ledger record
// within a database transaction. Typed AccountID and
// LedgerEventType values are flattened to strings for the
// sqlc parameter type, which mirrors the underlying TEXT
// columns in the ledger_entries table.
func (s *LedgerStoreDB) InsertLedgerEntry(
	ctx context.Context, entry LedgerEntry) error {

	debit := string(entry.DebitAccount)
	credit := string(entry.CreditAccount)
	event := string(entry.EventType)

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertLedgerEntry(
				ctx, sqlc.InsertLedgerEntryParams{
					DebitAccount:  debit,
					CreditAccount: credit,
					AmountSat:     entry.AmountSat,
					RoundID:       entry.RoundID,
					EventType:     event,
					Description:   entry.Description,
					CreatedAt:     entry.CreatedAt,
				},
			)
		},
	)
}
