package db

import (
	"context"
	"database/sql"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
)

// Compile-time check that LedgerStoreDB implements
// ledger.LedgerStore.
var _ ledger.LedgerStore = (*LedgerStoreDB)(nil)

// LedgerStoreDB bridges the ledger.LedgerStore interface to
// the sqlc-generated queries. This adapter converts LedgerEntry
// to sqlc.InsertClientLedgerEntryParams and wraps all operations
// in ExecTx for transactional safety.
//
// Beyond the ledger.LedgerStore interface, LedgerStoreDB
// also provides query methods (GetAccountBalance, ListLedgerEntries,
// etc.) used by the daemon RPC layer.
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

// InsertLedgerEntry persists a client-side double-entry ledger record.
// When ctx carries a durable-actor transaction (actor.TxFromContext),
// TransactionExecutor.ExecTx joins that outer tx rather than opening
// a fresh one, so multiple invocations from within a single actor
// handler commit atomically alongside the mailbox ack. The underlying
// InsertClientLedgerEntry query uses ON CONFLICT DO NOTHING against
// the partial unique indexes on round_id, session_id, and
// idempotency_key so redelivery of an already-persisted message
// silently dedupes instead of raising a constraint violation.
func (s *LedgerStoreDB) InsertLedgerEntry(ctx context.Context,
	entry ledger.LedgerEntry) error {

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertClientLedgerEntry(
				ctx, sqlc.InsertClientLedgerEntryParams{
					DebitAccount:  entry.DebitAccount,
					CreditAccount: entry.CreditAccount,
					AmountSat:     entry.AmountSat,
					RoundID:       entry.RoundID,
					RoundUuid: roundUUIDText(
						entry.RoundID,
					),
					SessionID:      entry.SessionID,
					EventType:      entry.EventType,
					Description:    entry.Description,
					CreatedAt:      entry.CreatedAt,
					IdempotencyKey: entry.IdempotencyKey,
					ChainTxid:      entry.ChainTxid,
					ChainVout: sqlInt32Ptr(
						entry.ChainVout,
					),
					ConfirmationHeight: sqlInt32Ptr(
						entry.ConfirmationHeight,
					),
				},
			)
		},
	)
}

// roundUUIDText mirrors a raw 16-byte ledger round_id into the canonical
// lowercase UUID string stored by rounds.round_id and vtxos.forfeit_round_id,
// so ledger rows are joinable against those tables in portable SQL. A nil or
// non-16-byte input yields NULL, mirroring the round_id column's nullability.
func roundUUIDText(roundID []byte) sql.NullString {
	if len(roundID) != 16 {
		return sql.NullString{}
	}

	var id uuid.UUID
	copy(id[:], roundID)

	return sql.NullString{
		String: id.String(),
		Valid:  true,
	}
}

// sqlInt32Ptr converts an optional int32 pointer to the nullable sqlc shape
// used by ledger chain metadata columns.
func sqlInt32Ptr(v *int32) sql.NullInt32 {
	if v == nil {
		return sql.NullInt32{}
	}

	return sql.NullInt32{
		Int32: *v,
		Valid: true,
	}
}

// GetConfirmedExitCost returns the confirmed on-chain cost the ledger booked
// for a unilateral exit of the given VTXO outpoint (the onchain_fee_paid leg
// unroll emits after the final sweep confirms). Zero when no exit-cost leg
// exists — the exit has not confirmed, or predates exit-cost accounting.
func (s *LedgerStoreDB) GetConfirmedExitCost(ctx context.Context,
	outpoint wire.OutPoint) (int64, error) {

	key := ledger.ExitIdempotencyKey(outpoint.Hash, outpoint.Index)

	var cost int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			cost, txErr = qtx.GetConfirmedExitCost(ctx, key)

			return txErr
		},
	)

	return cost, err
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
func (s *LedgerStoreDB) GetTotalOperatorFeesPaid(ctx context.Context) (int64,
	error) {

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
func (s *LedgerStoreDB) ListLedgerEntries(ctx context.Context, limit,
	offset int32) ([]sqlc.LedgerEntry, error) {

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

// ListLedgerEntriesWithFeesTotal returns a paginated list of ledger
// entries together with the cumulative operator-fees-paid total, both
// observed inside the same read transaction. Reading both in one tx
// guarantees the returned page and total are mutually consistent: a
// concurrent insert cannot land between the two queries and produce a
// total that already counts an entry not yet visible on the page.
func (s *LedgerStoreDB) ListLedgerEntriesWithFeesTotal(ctx context.Context,
	limit, offset int32) ([]sqlc.LedgerEntry, int64, error) {

	var (
		entries []sqlc.LedgerEntry
		total   int64
	)
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
			if txErr != nil {
				return txErr
			}

			total, txErr = qtx.GetTotalOperatorFeesPaid(ctx)

			return txErr
		},
	)

	return entries, total, err
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

// ListTransactionHistory returns a unified newest-first page of
// ledger-backed activity and tracked boarding sweep transactions. The
// type and timestamp filters are applied in SQL before pagination so a
// filtered request never returns an empty page just because earlier
// unfiltered rows did not match.
func (s *LedgerStoreDB) ListTransactionHistory(ctx context.Context,
	typeFilter string, fromUnixS, toUnixS int64, limit, offset int32) (
	[]sqlc.ListTransactionHistoryRow, error) {

	var entries []sqlc.ListTransactionHistoryRow
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListTransactionHistory(
				ctx, sqlc.ListTransactionHistoryParams{
					TypeFilter: typeFilter,
					FromUnixS:  fromUnixS,
					ToUnixS:    toUnixS,
					PageLimit:  limit,
					PageOffset: offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// CountLedgerEntries returns the total number of ledger entries within
// a read transaction.
func (s *LedgerStoreDB) CountLedgerEntries(ctx context.Context) (int64, error) {
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
func (s *LedgerStoreDB) ListAccounts(ctx context.Context) ([]sqlc.Account,
	error) {

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
