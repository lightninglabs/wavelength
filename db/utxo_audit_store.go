package db

import (
	"context"
	"database/sql"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
)

// Compile-time check that UTXOAuditStoreDB implements
// ledger.UTXOAuditStore.
var _ ledger.UTXOAuditStore = (*UTXOAuditStoreDB)(nil)

// UTXOAuditStoreDB bridges the ledger.UTXOAuditStore
// interface to the sqlc-generated queries. This adapter converts
// UTXOAuditEntry to sqlc.InsertWalletUTXOLogParams and wraps
// all operations in ExecTx for transactional safety.
//
// Beyond the ledger.UTXOAuditStore interface,
// UTXOAuditStoreDB also provides query methods
// (ListUTXOAuditEntries, etc.) used by the daemon RPC layer.
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
func (s *UTXOAuditStoreDB) InsertUTXOAuditEntry(ctx context.Context,
	entry ledger.UTXOAuditEntry) error {

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
func (s *UTXOAuditStoreDB) ListUTXOAuditEntries(ctx context.Context, limit,
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
func (s *UTXOAuditStoreDB) ListUTXOAuditEntriesByBlock(ctx context.Context,
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
	ctx context.Context, classification string, limit, offset int32) (
	[]sqlc.WalletUtxoLog, error) {

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
func (s *UTXOAuditStoreDB) CountUTXOAuditEntries(ctx context.Context) (int64,
	error) {

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
