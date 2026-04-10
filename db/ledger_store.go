package db

import (
	"context"
	"database/sql"

	"github.com/lightninglabs/darepo/db/sqlc"
)

// LedgerEntry is the domain-level representation of a
// double-entry ledger record. It mirrors the shape of
// sqlc.InsertLedgerEntryParams but lives in the db package so
// callers do not need to depend on the generated sqlc types.
//
// NOTE: this is a local stand-in for the future
// fees.LedgerEntry. Once the fees package lands later in the
// stack, this type can be replaced by an alias or removed in
// favor of fees.LedgerEntry, at which point the LedgerStore
// interface and compile-time assertion will be reintroduced
// alongside it.
type LedgerEntry struct {
	// DebitAccount is the account_id that the amount is debited
	// from.
	DebitAccount string

	// CreditAccount is the account_id that the amount is
	// credited to.
	CreditAccount string

	// AmountSat is the strictly positive amount in satoshis.
	AmountSat int64

	// RoundID optionally associates the entry with a round.
	RoundID []byte

	// EventType classifies the entry per the seeded
	// ledger_event_types catalog (e.g. "boarding_fee").
	EventType string

	// Description is a free-form human-readable note.
	Description string

	// CreatedAt is the unix-second timestamp the entry was
	// recorded.
	CreatedAt int64
}

// LedgerStoreDB bridges the domain-level ledger entry type to
// the sqlc-generated queries. This adapter converts LedgerEntry
// to sqlc.InsertLedgerEntryParams and wraps all writes in
// ExecTx for transactional safety.
type LedgerStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

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
// within a database transaction.
func (s *LedgerStoreDB) InsertLedgerEntry(
	ctx context.Context, entry LedgerEntry) error {

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertLedgerEntry(
				ctx, sqlc.InsertLedgerEntryParams{
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
