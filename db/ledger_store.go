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
// LedgerEventType values are flattened to strings, and
// btcutil.Amount / time.Time are flattened to int64 / Unix
// seconds to match the underlying sqlc column types.
//
// The insert uses ON CONFLICT DO NOTHING on the partial unique
// (idempotency_key, event_type, debit_account, credit_account)
// index, so at-least-once mailbox replay with a stable
// idempotency key is a silent no-op rather than a constraint
// violation. The rowcount from sqlc is discarded: the caller
// does not distinguish "inserted" from "silently deduped"
// today. If a future caller needs to surface that signal it
// can plumb the return up without changing the schema.
func (s *LedgerStoreDB) InsertLedgerEntry(
	ctx context.Context, entry LedgerEntry) error {

	debit := string(entry.DebitAccount)
	credit := string(entry.CreditAccount)
	event := string(entry.EventType)

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			_, err := qtx.InsertLedgerEntry(
				ctx, sqlc.InsertLedgerEntryParams{
					DebitAccount:   debit,
					CreditAccount:  credit,
					AmountSat:      int64(entry.Amount),
					RoundID:        entry.RoundID,
					SessionID:      entry.SessionID,
					IdempotencyKey: entry.IdempotencyKey,
					EventType:      event,
					Description:    entry.Description,
					CreatedAt:      entry.CreatedAt.Unix(),
				},
			)

			return err
		},
	)
}
