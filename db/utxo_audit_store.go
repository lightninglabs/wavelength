package db

import (
	"context"
	"database/sql"

	"github.com/lightninglabs/darepo-client/db/sqlc"
)

// UTXOAuditEntry is a domain-level representation of a wallet
// UTXO audit log record. Each row records a single UTXO being
// created or spent, classified by its likely cause.
type UTXOAuditEntry struct {
	// OutpointHash is the 32-byte transaction hash.
	OutpointHash []byte

	// OutpointIndex is the output index within the transaction.
	OutpointIndex int32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// Event is "created" or "spent".
	Event string

	// BlockHeight is the block where this change occurred.
	BlockHeight int32

	// ClassifiedAs categorizes the UTXO event (e.g.
	// "deposit", "round_funding", "sweep_return", "change",
	// "unknown").
	ClassifiedAs string

	// CreatedAt is the Unix timestamp when this entry was
	// recorded.
	CreatedAt int64
}

// UTXOAuditStore is the persistence interface for the client-side
// wallet UTXO audit log.
type UTXOAuditStore interface {
	// InsertUTXOAuditEntry persists a single UTXO audit log
	// record.
	InsertUTXOAuditEntry(ctx context.Context,
		entry UTXOAuditEntry) error

	// ListUTXOAuditEntries returns a paginated list of entries
	// ordered by creation time descending.
	ListUTXOAuditEntries(ctx context.Context, limit,
		offset int32) ([]sqlc.WalletUtxoLog, error)

	// ListUTXOAuditEntriesByBlock returns all entries for a
	// given block height.
	ListUTXOAuditEntriesByBlock(ctx context.Context,
		blockHeight int32) ([]sqlc.WalletUtxoLog, error)

	// ListUTXOAuditEntriesByClassification returns a paginated
	// list of entries filtered by classification.
	ListUTXOAuditEntriesByClassification(ctx context.Context,
		classification string, limit,
		offset int32) ([]sqlc.WalletUtxoLog, error)

	// CountUTXOAuditEntries returns the total number of entries.
	CountUTXOAuditEntries(ctx context.Context) (int64, error)
}

// Compile-time check that UTXOAuditStoreDB implements
// UTXOAuditStore.
var _ UTXOAuditStore = (*UTXOAuditStoreDB)(nil)

// UTXOAuditStoreDB bridges the UTXOAuditStore interface to the
// sqlc-generated queries. This adapter converts UTXOAuditEntry
// to sqlc.InsertWalletUTXOLogParams and wraps all operations in
// ExecTx for transactional safety.
type UTXOAuditStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewUTXOAuditStoreDB creates a new UTXOAuditStoreDB from a
// Store.
func NewUTXOAuditStoreDB(store *Store) *UTXOAuditStoreDB {
	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &UTXOAuditStoreDB{
		TransactionExecutor: txExec,
	}
}

// InsertUTXOAuditEntry persists a UTXO audit log record within
// a database transaction.
func (s *UTXOAuditStoreDB) InsertUTXOAuditEntry(
	ctx context.Context, entry UTXOAuditEntry) error {

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertWalletUTXOLog(
				ctx, sqlc.InsertWalletUTXOLogParams{
					OutpointHash:  entry.OutpointHash,
					OutpointIndex: entry.OutpointIndex,
					AmountSat:     entry.AmountSat,
					Event:         entry.Event,
					BlockHeight:   entry.BlockHeight,
					ClassifiedAs:  entry.ClassifiedAs,
					CreatedAt:     entry.CreatedAt,
				},
			)
		},
	)
}

// ListUTXOAuditEntries returns a paginated list of UTXO audit
// entries ordered by creation time within a read transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntries(
	ctx context.Context, limit,
	offset int32) ([]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLog(
				ctx, sqlc.ListWalletUTXOLogParams{
					Limit:  limit,
					Offset: offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// ListUTXOAuditEntriesByBlock returns all UTXO audit entries for
// a given block height within a read transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntriesByBlock(
	ctx context.Context,
	blockHeight int32) ([]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLogByBlock(
				ctx, blockHeight,
			)

			return txErr
		},
	)

	return entries, err
}

// ListUTXOAuditEntriesByClassification returns a paginated list
// of entries filtered by classification within a read
// transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntriesByClassification(
	ctx context.Context, classification string, limit,
	offset int32) ([]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLogByClassification(
				ctx,
				sqlc.ListWalletUTXOLogByClassificationParams{
					ClassifiedAs: classification,
					Limit:        limit,
					Offset:       offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// CountUTXOAuditEntries returns the total number of UTXO audit
// entries within a read transaction.
func (s *UTXOAuditStoreDB) CountUTXOAuditEntries(
	ctx context.Context) (int64, error) {

	var count int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			count, txErr = qtx.CountWalletUTXOLog(ctx)
			return txErr
		},
	)

	return count, err
}
