package db

import (
	"context"
	"database/sql"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/ledger"
)

// UTXOAuditStoreDB bridges the ledger actor's UTXO audit and
// snapshot-reconstruction seams to the sqlc-generated queries.
// The same adapter satisfies both the write-only audit sink and
// the read-only snapshot reader because the underlying rows
// live in one table (wallet_utxo_log): audit writes append
// 'created'/'spent' events and the reader reconstructs the
// current live UTXO set as "created without a paired spent".
// One adapter, one SQL table, one source of truth.
type UTXOAuditStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

// Compile-time assertions. Break loudly if the interface shape
// ever drifts from the ledger package.
var (
	_ ledger.UTXOAuditStore     = (*UTXOAuditStoreDB)(nil)
	_ ledger.UTXOSnapshotReader = (*UTXOAuditStoreDB)(nil)
)

// NewUTXOAuditStoreDB creates a new UTXOAuditStoreDB bound to
// the underlying Store's connection pool.
func NewUTXOAuditStoreDB(store *Store) *UTXOAuditStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &UTXOAuditStoreDB{
		TransactionExecutor: txExec,
	}
}

// InsertWalletUTXOLog persists a single wallet_utxo_log row.
// The sqlc query uses ON CONFLICT DO NOTHING against
// UNIQUE(outpoint_hash, outpoint_index, event) so retries and
// mailbox replay are silent no-ops. The first return value
// reports whether the row was newly inserted (1) or silently
// deduped (0); the diff loop uses this signal to decide
// whether a handler pre-insert already attributed the outpoint.
func (s *UTXOAuditStoreDB) InsertWalletUTXOLog(ctx context.Context,
	entry ledger.WalletUTXOLogEntry) (int64, error) {

	var rows int64
	err := s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			params := sqlc.InsertWalletUTXOLogParams{
				OutpointHash: entry.Outpoint.Hash[:],
				OutpointIndex: int32(
					entry.Outpoint.Index,
				),
				AmountSat:   int64(entry.Amount),
				Event:       string(entry.Event),
				BlockHeight: int32(entry.BlockHeight),
				ClassifiedAs: string(
					entry.Classification,
				),
				CreatedAt: entry.CreatedAt.Unix(),
				SourceID:  entry.SourceID,
			}

			var err error
			rows, err = qtx.InsertWalletUTXOLog(ctx, params)

			return err
		},
	)

	return rows, err
}

// PromotePendingWalletUTXOLog flips every 'pending' audit row
// whose block_height is strictly below the watermark into its
// terminal classification ('deposit' for created,
// 'withdrawal' for spent). Runs under a write transaction and
// returns the promoted rows so the classifier can book the
// corresponding external_* ledger legs in the same pass.
func (s *UTXOAuditStoreDB) PromotePendingWalletUTXOLog(
	ctx context.Context, watermark int64,
) ([]ledger.WalletUTXOLogEntry, error) {

	var promoted []ledger.WalletUTXOLogEntry
	err := s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			rows, err := qtx.PromotePendingWalletUTXOLog(
				ctx, int32(watermark),
			)
			if err != nil {
				return err
			}

			promoted = make(
				[]ledger.WalletUTXOLogEntry, 0, len(rows),
			)
			for _, r := range rows {
				hash, err := chainhash.NewHash(
					r.OutpointHash,
				)
				if err != nil {
					return err
				}

				promoted = append(
					promoted, ledger.WalletUTXOLogEntry{
						Outpoint: wire.OutPoint{
							Hash: *hash,
							Index: uint32(
								r.OutpointIndex,
							),
						},
						Amount: btcutil.Amount(
							r.AmountSat,
						),
						Event: ledger.UTXOAuditEvent(
							r.Event,
						),
						BlockHeight: int64(
							r.BlockHeight,
						),
						Classification: ledger.
							UTXOClassification(
							r.ClassifiedAs,
						),
						SourceID: r.SourceID,
					},
				)
			}

			return nil
		},
	)

	return promoted, err
}

// ListLiveWalletUTXOs reconstructs the treasury wallet's current
// UTXO set from the audit log so the ledger actor can rehydrate
// its in-memory snapshot across a restart. The second return
// value is the highest block_height observed among the live
// UTXOs, which callers can pass along as the "last processed"
// height (zero when the set is empty).
//
// Runs under a read-only transaction so concurrent writes to the
// audit log do not block.
func (s *UTXOAuditStoreDB) ListLiveWalletUTXOs(
	ctx context.Context) ([]ledger.WalletUTXO, int64, error) {

	var (
		utxos       []ledger.WalletUTXO
		maxBlockHgt int64
	)

	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			rows, err := qtx.ListLiveWalletUTXOs(ctx)
			if err != nil {
				return err
			}

			utxos = make([]ledger.WalletUTXO, 0, len(rows))
			for _, r := range rows {
				hash, err := chainhash.NewHash(
					r.OutpointHash,
				)
				if err != nil {
					return err
				}

				utxos = append(utxos, ledger.WalletUTXO{
					Outpoint: wire.OutPoint{
						Hash:  *hash,
						Index: uint32(r.OutpointIndex),
					},
					Amount: btcutil.Amount(r.AmountSat),
				})

				if int64(r.BlockHeight) > maxBlockHgt {
					maxBlockHgt = int64(r.BlockHeight)
				}
			}

			return nil
		},
	)
	if err != nil {
		return nil, 0, err
	}

	return utxos, maxBlockHgt, nil
}

// CountAuditRows returns the total number of rows in the
// wallet_utxo_log table. The ledger actor's startup reseed
// uses this to distinguish a fresh install (no rows ever
// written, seeding is legitimate) from a running deployment
// whose wallet is temporarily empty (rows exist, but the live
// set happens to be zero). Without this split, an empty wallet
// at restart would silently re-enter seeding and drop
// attribution for the first post-restart external deposit.
//
// Runs under a read-only transaction for the same reason as
// ListLiveWalletUTXOs -- concurrent writes to the audit log
// must not block a startup read.
func (s *UTXOAuditStoreDB) CountAuditRows(
	ctx context.Context) (int64, error) {

	var count int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var err error
			count, err = qtx.CountWalletUTXOLog(ctx)

			return err
		},
	)
	if err != nil {
		return 0, err
	}

	return count, nil
}
