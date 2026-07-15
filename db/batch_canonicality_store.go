package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// BatchCanonicalityStore groups the generated SQL methods needed to persist
// the batch canonicality data model.
//
//nolint:interfacebloat // One handle keeps all canonicality ExecTx closures.
type BatchCanonicalityStore interface {
	UpsertBatchCanonicality(ctx context.Context,
		arg sqlc.UpsertBatchCanonicalityParams) error

	GetBatchCanonicality(ctx context.Context,
		batchTxid []byte) (sqlc.BatchCanonicality, error)

	ListBatchCanonicalityByState(ctx context.Context,
		state int32) ([]sqlc.BatchCanonicality, error)

	UpdateBatchCanonicalityState(ctx context.Context,
		arg sqlc.UpdateBatchCanonicalityStateParams) error

	RecordBatchConfirmation(ctx context.Context,
		arg sqlc.RecordBatchConfirmationParams) error

	ClearBatchConfirmation(ctx context.Context,
		arg sqlc.ClearBatchConfirmationParams) error

	InsertBatchConsumedInput(ctx context.Context,
		arg sqlc.InsertBatchConsumedInputParams) error

	DeleteBatchConsumedInputs(ctx context.Context, batchTxid []byte) error

	ListBatchConsumedInputs(ctx context.Context,
		batchTxid []byte) ([]sqlc.ListBatchConsumedInputsRow, error)

	RecordBatchInputConflict(ctx context.Context,
		arg sqlc.RecordBatchInputConflictParams) error

	FindBatchesByConsumedOutpoint(ctx context.Context,
		arg sqlc.FindBatchesByConsumedOutpointParams) ([][]byte, error)

	InsertBatchDependentVTXO(ctx context.Context,
		arg sqlc.InsertBatchDependentVTXOParams) error

	DeleteBatchDependentVTXOs(ctx context.Context, batchTxid []byte) error

	ListBatchDependentVTXOs(ctx context.Context,
		batchTxid []byte) ([]sqlc.ListBatchDependentVTXOsRow, error)

	InsertProvisionalConsumer(ctx context.Context,
		arg sqlc.InsertProvisionalConsumerParams) error

	ListProvisionalConsumersForBatch(ctx context.Context,
		consumerBatchTxid []byte) (
		[]sqlc.ListProvisionalConsumersForBatchRow, error)

	DeleteProvisionalConsumersForBatch(ctx context.Context,
		consumerBatchTxid []byte) error

	ListVTXOsForCanonicalityBackfill(ctx context.Context) (
		[]sqlc.ListVTXOsForCanonicalityBackfillRow, error)
}

// BatchedBatchCanonicalityStore combines the query surface with batched
// transaction execution.
type BatchedBatchCanonicalityStore interface {
	BatchCanonicalityStore
	BatchedTx[BatchCanonicalityStore]
}

// BatchCanonicalityPersistenceStore persists the durable batch canonicality
// data model: per-batch canonicality records, the inputs each batch consumes,
// the VTXOs it anchors, and the reverse-dependency edges used to restore a
// provisionally consumed VTXO. It is behavior-free; interpretation lives in
// the batch canonicality manager.
type BatchCanonicalityPersistenceStore struct {
	db    BatchedBatchCanonicalityStore
	clock clock.Clock
}

// NewBatchCanonicalityPersistenceStore creates a batch canonicality store
// using the transaction executor pattern.
func NewBatchCanonicalityPersistenceStore(db BatchedBatchCanonicalityStore,
	clk clock.Clock) *BatchCanonicalityPersistenceStore {

	return &BatchCanonicalityPersistenceStore{
		db:    db,
		clock: clk,
	}
}

// UpsertBatch inserts or replaces the canonicality record for a batch,
// including its consumed inputs and dependent VTXOs. The input and dependent
// sets are replaced wholesale (delete-then-insert) so the persisted edges
// always match the supplied record.
func (s *BatchCanonicalityPersistenceStore) UpsertBatch(ctx context.Context,
	record *batchcanon.Record) error {

	now := s.clock.Now().Unix()
	txid := record.BatchTxID
	pkScript := record.ConfirmationPkScript

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		err := q.UpsertBatchCanonicality(
			ctx, sqlc.UpsertBatchCanonicalityParams{
				BatchTxid: txid[:],
				State:     int32(record.State),
				ConfirmationHeight: optionToNullInt32(
					record.ConfirmationHeight,
				),
				ConfirmationBlockHash: optionHashToBytes(
					record.ConfirmationBlock,
				),
				CsvExpiryDelta:       record.CSVExpiryDelta,
				PolicyState:          int32(record.PolicyState),
				ConfirmationPkScript: pkScript,
				CreatedAt:            now,
				UpdatedAt:            now,
			},
		)
		if err != nil {
			return err
		}

		// Replace the consumed-input set.
		if err := q.DeleteBatchConsumedInputs(
			ctx, txid[:],
		); err != nil {
			return err
		}
		for _, in := range record.ConsumedInputs {
			err := q.InsertBatchConsumedInput(
				ctx, sqlc.InsertBatchConsumedInputParams{
					BatchTxid:     txid[:],
					InputHash:     in.Outpoint.Hash[:],
					InputIndex:    int32(in.Outpoint.Index),
					InputPkScript: in.PkScript,
				},
			)
			if err != nil {
				return err
			}
		}

		// Replace the dependent-VTXO set.
		err = q.DeleteBatchDependentVTXOs(ctx, txid[:])
		if err != nil {
			return err
		}

		return insertDependentVTXOs(ctx, q, txid, record.DependentVTXOs)
	})
}

// insertDependentVTXOs links each dependent VTXO outpoint to a batch txid.
// Shared by UpsertBatch and BackfillFromVTXOs so neither nests the insert
// loop deeply enough to overflow the line-length budget.
func insertDependentVTXOs(ctx context.Context, q BatchCanonicalityStore,
	txid chainhash.Hash, deps []wire.OutPoint) error {

	for _, dep := range deps {
		err := q.InsertBatchDependentVTXO(
			ctx, sqlc.InsertBatchDependentVTXOParams{
				BatchTxid:         txid[:],
				VtxoOutpointHash:  dep.Hash[:],
				VtxoOutpointIndex: int32(dep.Index),
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetBatch returns the canonicality record for a batch txid, hydrating its
// consumed inputs and dependent VTXOs. It returns batchcanon.ErrBatchNotFound
// when no record exists.
func (s *BatchCanonicalityPersistenceStore) GetBatch(ctx context.Context,
	txid chainhash.Hash) (*batchcanon.Record, error) {

	var record *batchcanon.Record

	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		row, err := q.GetBatchCanonicality(ctx, txid[:])
		if errors.Is(err, sql.ErrNoRows) {
			return batchcanon.ErrBatchNotFound
		}
		if err != nil {
			return err
		}

		rec, err := s.hydrateRecord(ctx, q, row)
		if err != nil {
			return err
		}
		record = rec

		return nil
	})

	return record, err
}

// ListBatchesByState returns every batch currently in the given state,
// hydrating each record's consumed inputs and dependent VTXOs.
func (s *BatchCanonicalityPersistenceStore) ListBatchesByState(
	ctx context.Context, state batchcanon.State) ([]*batchcanon.Record,
	error) {

	var records []*batchcanon.Record

	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.ListBatchCanonicalityByState(ctx, int32(state))
		if err != nil {
			return err
		}

		records = make([]*batchcanon.Record, 0, len(rows))
		for _, row := range rows {
			rec, err := s.hydrateRecord(ctx, q, row)
			if err != nil {
				return err
			}
			records = append(records, rec)
		}

		return nil
	})

	return records, err
}

// UpdateBatchState transitions a batch to a new canonicality state.
func (s *BatchCanonicalityPersistenceStore) UpdateBatchState(
	ctx context.Context, txid chainhash.Hash,
	state batchcanon.State) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.UpdateBatchCanonicalityState(
			ctx, sqlc.UpdateBatchCanonicalityStateParams{
				BatchTxid: txid[:],
				State:     int32(state),
				UpdatedAt: s.clock.Now().Unix(),
			},
		)
	})
}

// RecordConfirmation records the best-chain height and block hash at which
// the batch tx is confirmed.
func (s *BatchCanonicalityPersistenceStore) RecordConfirmation(
	ctx context.Context, txid chainhash.Hash, height int32,
	block chainhash.Hash) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.RecordBatchConfirmation(
			ctx, sqlc.RecordBatchConfirmationParams{
				BatchTxid: txid[:],
				ConfirmationHeight: sql.NullInt32{
					Int32: height,
					Valid: true,
				},
				ConfirmationBlockHash: block[:],
				UpdatedAt:             s.clock.Now().Unix(),
			},
		)
	})
}

// ClearConfirmation clears the confirmation observation for a batch.
func (s *BatchCanonicalityPersistenceStore) ClearConfirmation(
	ctx context.Context, txid chainhash.Hash) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.ClearBatchConfirmation(
			ctx, sqlc.ClearBatchConfirmationParams{
				BatchTxid: txid[:],
				UpdatedAt: s.clock.Now().Unix(),
			},
		)
	})
}

// RecordInputConflict persists the observed conflict status of one consumed
// input, so restart reconciliation can rebuild the per-input conflict view.
func (s *BatchCanonicalityPersistenceStore) RecordInputConflict(
	ctx context.Context, batchTxid chainhash.Hash, outpoint wire.OutPoint,
	conflicting, conflictFinal bool) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.RecordBatchInputConflict(
			ctx, sqlc.RecordBatchInputConflictParams{
				BatchTxid:     batchTxid[:],
				InputHash:     outpoint.Hash[:],
				InputIndex:    int32(outpoint.Index),
				Conflicting:   boolToInt32(conflicting),
				ConflictFinal: boolToInt32(conflictFinal),
			},
		)
	})
}

// FindBatchesConsumingOutpoint returns the txids of every recorded batch that
// consumes the given outpoint.
func (s *BatchCanonicalityPersistenceStore) FindBatchesConsumingOutpoint(
	ctx context.Context, outpoint wire.OutPoint) ([]chainhash.Hash, error) {

	var txids []chainhash.Hash

	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.FindBatchesByConsumedOutpoint(
			ctx, sqlc.FindBatchesByConsumedOutpointParams{
				InputHash:  outpoint.Hash[:],
				InputIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return err
		}

		txids = make([]chainhash.Hash, 0, len(rows))
		for _, raw := range rows {
			hash, err := chainhash.NewHash(raw)
			if err != nil {
				return err
			}
			txids = append(txids, *hash)
		}

		return nil
	})

	return txids, err
}

// AddProvisionalConsumer records a reverse-dependency edge: the given VTXO
// outpoint is provisionally consumed by the given consumer batch.
func (s *BatchCanonicalityPersistenceStore) AddProvisionalConsumer(
	ctx context.Context, consumedVTXO wire.OutPoint,
	consumerBatch chainhash.Hash) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.InsertProvisionalConsumer(
			ctx, sqlc.InsertProvisionalConsumerParams{
				ConsumedVtxoHash:  consumedVTXO.Hash[:],
				ConsumedVtxoIndex: int32(consumedVTXO.Index),
				ConsumerBatchTxid: consumerBatch[:],
				CreatedAt:         s.clock.Now().Unix(),
			},
		)
	})
}

// ListProvisionalConsumersForBatch returns the VTXO outpoints that the given
// consumer batch provisionally consumes.
func (s *BatchCanonicalityPersistenceStore) ListProvisionalConsumersForBatch(
	ctx context.Context, consumerBatch chainhash.Hash) ([]wire.OutPoint,
	error) {

	var outpoints []wire.OutPoint

	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.ListProvisionalConsumersForBatch(
			ctx, consumerBatch[:],
		)
		if err != nil {
			return err
		}

		outpoints = make([]wire.OutPoint, 0, len(rows))
		for _, row := range rows {
			hash, err := chainhash.NewHash(row.ConsumedVtxoHash)
			if err != nil {
				return err
			}
			outpoints = append(outpoints, wire.OutPoint{
				Hash:  *hash,
				Index: uint32(row.ConsumedVtxoIndex),
			})
		}

		return nil
	})

	return outpoints, err
}

// DeleteProvisionalConsumersForBatch removes every reverse-dependency edge for
// the given consumer batch.
func (s *BatchCanonicalityPersistenceStore) DeleteProvisionalConsumersForBatch(
	ctx context.Context, consumerBatch chainhash.Hash) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return q.DeleteProvisionalConsumersForBatch(
			ctx, consumerBatch[:],
		)
	})
}

// backfillGroup accumulates the per-batch data derived from VTXO rows during
// backfill: the batch-level expiry and creation height (shared by every VTXO
// in the batch) plus the set of dependent VTXO outpoints.
type backfillGroup struct {
	batchExpiry   int32
	createdHeight int32
	dependents    []wire.OutPoint
}

// BackfillFromVTXOs derives an initial canonicality record for every distinct
// batch (commitment) txid present in the VTXO store that does not already have
// a record, so an upgrading node carries forward the batches its existing
// VTXOs depend on. It is idempotent: batches that already have a record are
// left untouched, so a re-run never clobbers state the manager has since
// advanced. It returns the number of batch records created.
//
// Classification uses the supplied best height and finality depth: a batch
// confirmed at least finalityDepth deep (inclusive) is finalized, otherwise
// provisional. The CSV-relative expiry delta is recovered as
// batch_expiry - created_height so the derived effective expiry matches the
// original absolute batch_expiry while remaining reorg-recomputable. Batches
// whose VTXOs carry no positive creation height are skipped: without a
// confirmation observation the manager will create them on first sight.
func (s *BatchCanonicalityPersistenceStore) BackfillFromVTXOs(
	ctx context.Context, bestHeight int32, finalityDepth uint32) (int,
	error) {

	now := s.clock.Now().Unix()
	created := 0

	err := s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.ListVTXOsForCanonicalityBackfill(ctx)
		if err != nil {
			return err
		}

		// Group the VTXO rows by commitment (batch) txid.
		groups := make(map[chainhash.Hash]*backfillGroup)
		for _, row := range rows {
			txid, err := chainhash.NewHash(row.CommitmentTxid)
			if err != nil {
				return err
			}
			vtxoHash, err := chainhash.NewHash(row.OutpointHash)
			if err != nil {
				return err
			}

			g, ok := groups[*txid]
			if !ok {
				g = &backfillGroup{
					batchExpiry:   row.BatchExpiry,
					createdHeight: row.CreatedHeight,
				}
				groups[*txid] = g
			}
			g.dependents = append(g.dependents, wire.OutPoint{
				Hash:  *vtxoHash,
				Index: uint32(row.OutpointIndex),
			})
		}

		for txid, g := range groups {
			// Skip batches with no positive confirmation height:
			// the manager will create them once it observes a
			// confirmation.
			if g.createdHeight <= 0 {
				continue
			}

			// Skip batches that already have a record so a re-run
			// never overwrites advanced state.
			_, err := q.GetBatchCanonicality(ctx, txid[:])
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			csvDelta := g.batchExpiry - g.createdHeight
			if csvDelta < 0 {
				csvDelta = 0
			}

			state := batchcanon.StateProvisional
			depth := bestHeight - g.createdHeight + 1
			if depth >= int32(finalityDepth) {
				state = batchcanon.StateFinalized
			}

			err = q.UpsertBatchCanonicality(
				ctx, sqlc.UpsertBatchCanonicalityParams{
					BatchTxid: txid[:],
					State:     int32(state),
					ConfirmationHeight: sql.NullInt32{
						Int32: g.createdHeight,
						Valid: true,
					},
					// The confirming block hash is not
					// recorded on VTXO rows; it is an
					// observation attribute the manager
					// fills on its next confirmation
					// sighting.
					ConfirmationBlockHash: nil,
					CsvExpiryDelta:        csvDelta,
					PolicyState: int32(
						batchcanon.PolicyStateDefault,
					),
					CreatedAt: now,
					UpdatedAt: now,
				},
			)
			if err != nil {
				return err
			}

			err = insertDependentVTXOs(ctx, q, txid, g.dependents)
			if err != nil {
				return err
			}

			created++
		}

		return nil
	})

	return created, err
}

// hydrateRecord builds a batchcanon.Record from a canonicality row, loading
// its consumed inputs and dependent VTXOs through the same query handle (and
// therefore the same transaction).
func (s *BatchCanonicalityPersistenceStore) hydrateRecord(ctx context.Context,
	q BatchCanonicalityStore, row sqlc.BatchCanonicality) (
	*batchcanon.Record, error) {

	txid, err := chainhash.NewHash(row.BatchTxid)
	if err != nil {
		return nil, err
	}

	confBlock, err := bytesToOptionHash(row.ConfirmationBlockHash)
	if err != nil {
		return nil, err
	}

	inputRows, err := q.ListBatchConsumedInputs(ctx, row.BatchTxid)
	if err != nil {
		return nil, err
	}
	inputs := make([]batchcanon.ConsumedInput, 0, len(inputRows))
	for _, in := range inputRows {
		hash, err := chainhash.NewHash(in.InputHash)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, batchcanon.ConsumedInput{
			Outpoint: wire.OutPoint{
				Hash:  *hash,
				Index: uint32(in.InputIndex),
			},
			PkScript:      in.InputPkScript,
			Conflicting:   in.Conflicting != 0,
			ConflictFinal: in.ConflictFinal != 0,
		})
	}

	depRows, err := q.ListBatchDependentVTXOs(ctx, row.BatchTxid)
	if err != nil {
		return nil, err
	}
	deps := make([]wire.OutPoint, 0, len(depRows))
	for _, dep := range depRows {
		hash, err := chainhash.NewHash(dep.VtxoOutpointHash)
		if err != nil {
			return nil, err
		}
		deps = append(deps, wire.OutPoint{
			Hash:  *hash,
			Index: uint32(dep.VtxoOutpointIndex),
		})
	}

	return &batchcanon.Record{
		BatchTxID:            *txid,
		State:                batchcanon.State(row.State),
		ConfirmationHeight:   nullInt32ToOption(row.ConfirmationHeight),
		ConfirmationBlock:    confBlock,
		CSVExpiryDelta:       row.CsvExpiryDelta,
		PolicyState:          batchcanon.PolicyState(row.PolicyState),
		ConfirmationPkScript: row.ConfirmationPkScript,
		ConsumedInputs:       inputs,
		DependentVTXOs:       deps,
	}, nil
}

// boolToInt32 maps a Go bool to the 0/1 INTEGER encoding used for the
// consumed-input conflict flags.
func boolToInt32(b bool) int32 {
	if b {
		return 1
	}

	return 0
}

// optionToNullInt32 maps an optional int32 to a sql.NullInt32.
func optionToNullInt32(o fn.Option[int32]) sql.NullInt32 {
	if o.IsNone() {
		return sql.NullInt32{}
	}

	return sql.NullInt32{Int32: o.UnwrapOr(0), Valid: true}
}

// nullInt32ToOption maps a sql.NullInt32 back to an optional int32.
func nullInt32ToOption(n sql.NullInt32) fn.Option[int32] {
	if !n.Valid {
		return fn.None[int32]()
	}

	return fn.Some(n.Int32)
}

// optionHashToBytes maps an optional hash to its raw bytes, or nil when None.
func optionHashToBytes(o fn.Option[chainhash.Hash]) []byte {
	if o.IsNone() {
		return nil
	}

	h := o.UnwrapOr(chainhash.Hash{})

	return h[:]
}

// bytesToOptionHash maps a (possibly nil) raw hash to an optional hash. A nil
// or empty slice yields None; any other length is validated to 32 bytes.
func bytesToOptionHash(raw []byte) (fn.Option[chainhash.Hash], error) {
	if len(raw) == 0 {
		return fn.None[chainhash.Hash](), nil
	}

	hash, err := chainhash.NewHash(raw)
	if err != nil {
		return fn.None[chainhash.Hash](), err
	}

	return fn.Some(*hash), nil
}

// Compile-time check that the persistence store satisfies the domain Store
// interface.
var _ batchcanon.Store = (*BatchCanonicalityPersistenceStore)(nil)
