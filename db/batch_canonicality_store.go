package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	ApplyBatchCanonicalityObservation(ctx context.Context,
		arg sqlc.ApplyBatchCanonicalityObservationParams) (int64, error)

	UpsertBatchCanonicality(ctx context.Context,
		arg sqlc.UpsertBatchCanonicalityParams) error

	GetBatchCanonicality(ctx context.Context,
		batchTxid []byte) (sqlc.BatchCanonicality, error)

	ListBatchCanonicalityByState(ctx context.Context,
		state int32) ([]sqlc.BatchCanonicality, error)

	BeginBatchCanonicalityReconcile(ctx context.Context,
		arg sqlc.BeginBatchCanonicalityReconcileParams) (
		sqlc.BatchCanonicality, error)

	MarkBatchCanonicalityReady(ctx context.Context,
		arg sqlc.MarkBatchCanonicalityReadyParams) (int64, error)

	QuarantineBatchCanonicality(ctx context.Context,
		arg sqlc.QuarantineBatchCanonicalityParams) error

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
		arg sqlc.RecordBatchInputConflictParams) (int64, error)

	FindBatchesByConsumedOutpoint(ctx context.Context,
		arg sqlc.FindBatchesByConsumedOutpointParams) ([][]byte, error)

	InsertBatchDependentVTXO(ctx context.Context,
		arg sqlc.InsertBatchDependentVTXOParams) error

	DeleteBatchDependentVTXOs(ctx context.Context, batchTxid []byte) error

	ListBatchDependentVTXOs(ctx context.Context,
		batchTxid []byte) ([]sqlc.ListBatchDependentVTXOsRow, error)

	InsertProvisionalConsumer(ctx context.Context,
		arg sqlc.InsertProvisionalConsumerParams) error

	GetProvisionalConsumer(ctx context.Context,
		arg sqlc.GetProvisionalConsumerParams) (int64, error)

	InsertConsumerCreatorLineage(ctx context.Context,
		arg sqlc.InsertConsumerCreatorLineageParams) error

	ListConsumerCreatorLineage(ctx context.Context,
		arg sqlc.ListConsumerCreatorLineageParams) ([][]byte, error)

	ListProvisionalConsumersForBatch(ctx context.Context,
		consumerBatchTxid []byte) (
		[]sqlc.ListProvisionalConsumersForBatchRow, error)

	ListPendingConsumerBatchesByCreator(ctx context.Context,
		creatorBatchTxid []byte) ([][]byte, error)

	DeleteProvisionalConsumer(ctx context.Context,
		arg sqlc.DeleteProvisionalConsumerParams) (int64, error)

	RestoreForfeitedVTXOForConsumer(ctx context.Context,
		arg sqlc.RestoreForfeitedVTXOForConsumerParams) (int64, error)

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

// RegisterBatch atomically persists a complete batch registration and its
// provisional-consumer edges. A repeated registration may add dependents or
// consumer edges, but it cannot change the batch output, expiry policy, or
// actual consumed-input set. Preserving that immutable evidence prevents a
// retry from deleting a watched input or resetting its durable conflict flags.
func (s *BatchCanonicalityPersistenceStore) RegisterBatch(ctx context.Context,
	record *batchcanon.Record,
	consumerEdges []batchcanon.ConsumerEdge) error {

	now := s.clock.Now().Unix()
	txid := record.BatchTxID

	registrationConflict := false
	err := s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		row, err := q.GetBatchCanonicality(ctx, txid[:])
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := replaceBatchRecord(
				ctx, q, record, now,
			); err != nil {
				return err
			}

		case err != nil:
			return err

		default:
			existing, err := s.hydrateRecord(ctx, q, row)
			if err != nil {
				return err
			}

			// Upgrade-created placeholders intentionally carry no
			// serialized transaction or input evidence and are
			// never Ready. The first authenticated producer
			// registration may complete such a row exactly once.
			// Start a fresh generation from Unseen and retain the
			// union of historical dependents; no age-derived
			// confirmation assumption survives the completion
			// boundary.
			if len(existing.BatchTx) == 0 &&
				existing.RegistrationStage !=
					batchcanon.RegistrationComplete {

				completed := *record
				completed.State = batchcanon.StateUnseen
				completed.RegistrationStage =
					batchcanon.RegistrationRegistering
				completed.ReadyGeneration = fn.None[uint64]()
				completed.ConfirmationHeight = fn.None[int32]()
				completed.ConfirmationBlock =
					fn.None[chainhash.Hash]()
				completed.ObservationGeneration =
					existing.ObservationGeneration + 1
				if completed.ObservationGeneration == 0 {
					completed.ObservationGeneration = 1
				}
				completed.Revision = existing.Revision + 1
				completed.DependentVTXOs = mergeOutpoints(
					existing.DependentVTXOs,
					record.DependentVTXOs,
				)
				if err := replaceBatchRecord(
					ctx, q, &completed, now,
				); err != nil {
					return err
				}

				break
			}

			if err := registrationMatches(
				existing, record,
			); err != nil {

				registrationConflict = true
				params :=
					sqlc.QuarantineBatchCanonicalityParams{
						BatchTxid: txid[:],
						UpdatedAt: now,
					}

				return q.QuarantineBatchCanonicality(
					ctx, params,
				)
			}

			// Registration evidence is immutable, but dependents
			// are a monotonic set: another locally-owned output can
			// later be proven to descend from the same
			// already-watched batch.
			if err := insertDependentVTXOs(
				ctx, q, txid, record.DependentVTXOs,
			); err != nil {
				return err
			}
		}

		err = insertConsumerEdges(ctx, q, txid, consumerEdges, now)
		if errors.Is(err, batchcanon.ErrRegistrationConflict) {
			registrationConflict = true

			return q.QuarantineBatchCanonicality(
				ctx, sqlc.QuarantineBatchCanonicalityParams{
					BatchTxid: txid[:],
					UpdatedAt: now,
				},
			)
		}

		return err
	})
	if err != nil {
		return err
	}
	if registrationConflict {
		return batchcanon.ErrRegistrationConflict
	}

	return nil
}

// mergeOutpoints returns the stable set union of two outpoint slices.
func mergeOutpoints(a, b []wire.OutPoint) []wire.OutPoint {
	seen := make(map[wire.OutPoint]struct{}, len(a)+len(b))
	merged := make([]wire.OutPoint, 0, len(a)+len(b))
	for _, outpoints := range [][]wire.OutPoint{a, b} {
		for _, outpoint := range outpoints {
			if _, ok := seen[outpoint]; ok {
				continue
			}
			seen[outpoint] = struct{}{}
			merged = append(merged, outpoint)
		}
	}

	return merged
}

// BeginReconcile closes admission and advances the observation generation in
// one transaction before the caller arms any chain watch.
func (s *BatchCanonicalityPersistenceStore) BeginReconcile(ctx context.Context,
	txid chainhash.Hash) (*batchcanon.Record, error) {

	var record *batchcanon.Record
	err := s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		row, err := q.BeginBatchCanonicalityReconcile(
			ctx, sqlc.BeginBatchCanonicalityReconcileParams{
				BatchTxid: txid[:],
				UpdatedAt: s.clock.Now().Unix(),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return batchcanon.ErrBatchNotFound
		}
		if err != nil {
			return err
		}

		record, err = s.hydrateRecord(ctx, q, row)

		return err
	})

	return record, err
}

// MarkReady opens admission for a generation-consistent snapshot.
func (s *BatchCanonicalityPersistenceStore) MarkReady(ctx context.Context,
	txid chainhash.Hash, generation uint64) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.MarkBatchCanonicalityReady(
			ctx, sqlc.MarkBatchCanonicalityReadyParams{
				BatchTxid: txid[:],
				ReadyGeneration: sql.NullInt64{
					Int64: int64(generation),
					Valid: true,
				},
				UpdatedAt: s.clock.Now().Unix(),
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("stale batch readiness generation %d",
				generation)
		}

		return nil
	})
}

// ApplyObservation atomically writes one full chain-observation snapshot. It
// first verifies that the snapshot names exactly the immutable input set, then
// updates every input and the generation-guarded batch row in one transaction.
func (s *BatchCanonicalityPersistenceStore) ApplyObservation(
	ctx context.Context, snapshot *batchcanon.ObservationSnapshot) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		persistedInputs, err := q.ListBatchConsumedInputs(
			ctx, snapshot.BatchTxID[:],
		)
		if err != nil {
			return err
		}
		if len(persistedInputs) != len(snapshot.Inputs) {
			return fmt.Errorf("batch observation input count " +
				"changed")
		}

		expected := make(
			map[wire.OutPoint]struct{}, len(persistedInputs),
		)
		for _, input := range persistedInputs {
			hash, err := chainhash.NewHash(input.InputHash)
			if err != nil {
				return err
			}
			expected[wire.OutPoint{
				Hash:  *hash,
				Index: uint32(input.InputIndex),
			}] = struct{}{}
		}

		seen := make(map[wire.OutPoint]struct{}, len(snapshot.Inputs))
		for _, input := range snapshot.Inputs {
			if _, ok := expected[input.Outpoint]; !ok {
				return fmt.Errorf("batch observation contains "+
					"unknown input %s", input.Outpoint)
			}
			if _, duplicate := seen[input.Outpoint]; duplicate {
				return fmt.Errorf("batch observation "+
					"duplicates input %s", input.Outpoint)
			}
			seen[input.Outpoint] = struct{}{}

			rows, err := q.RecordBatchInputConflict(
				ctx, sqlc.RecordBatchInputConflictParams{
					BatchTxid: snapshot.BatchTxID[:],
					InputHash: input.Outpoint.Hash[:],
					InputIndex: int32(
						input.Outpoint.Index,
					),
					Conflicting: boolToInt32(
						input.Conflicting,
					),
					ConflictFinal: boolToInt32(
						input.ConflictFinal,
					),
				},
			)
			if err != nil {
				return err
			}
			if rows != 1 {
				return fmt.Errorf("batch observation input "+
					"%s missing", input.Outpoint)
			}
		}

		readyGeneration := sql.NullInt64{}
		if snapshot.Ready {
			readyGeneration = sql.NullInt64{
				Int64: int64(snapshot.Generation),
				Valid: true,
			}
		}
		rows, err := q.ApplyBatchCanonicalityObservation(
			ctx, sqlc.ApplyBatchCanonicalityObservationParams{
				BatchTxid: snapshot.BatchTxID[:],
				ObservationGeneration: int64(
					snapshot.Generation,
				),
				State: int32(snapshot.State),
				ConfirmationHeight: optionToNullInt32(
					snapshot.ConfirmationHeight,
				),
				ConfirmationBlockHash: optionHashToBytes(
					snapshot.ConfirmationBlock,
				),
				ReadyGeneration: readyGeneration,
				UpdatedAt:       s.clock.Now().Unix(),
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("stale or quarantined batch "+
				"observation generation %d",
				snapshot.Generation)
		}

		return nil
	})
}

// UpsertBatch inserts or replaces the canonicality record for a batch,
// including its consumed inputs and dependent VTXOs. The input and dependent
// sets are replaced wholesale (delete-then-insert) so the persisted edges
// always match the supplied record.
func (s *BatchCanonicalityPersistenceStore) UpsertBatch(ctx context.Context,
	record *batchcanon.Record) error {

	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		return replaceBatchRecord(ctx, q, record, now)
	})
}

// replaceBatchRecord writes a whole record inside the caller's transaction.
// It is used for explicit migration/backfill rewrites and for the first insert
// of RegisterBatch; ordinary repeat registration never calls it.
func replaceBatchRecord(ctx context.Context, q BatchCanonicalityStore,
	record *batchcanon.Record, now int64) error {

	txid := record.BatchTxID
	observationGeneration := record.ObservationGeneration
	if observationGeneration == 0 {
		observationGeneration = 1
	}
	err := q.UpsertBatchCanonicality(
		ctx, sqlc.UpsertBatchCanonicalityParams{
			BatchTxid: txid[:],
			BatchTx:   record.BatchTx,
			BatchOutputIndex: batchOutputIndexToNullInt32(
				record.BatchTx, record.BatchOutputIndex,
			),
			State:                 int32(record.State),
			RegistrationStage:     int32(record.RegistrationStage),
			ObservationGeneration: int64(observationGeneration),
			ReadyGeneration: optionUint64ToNullInt64(
				record.ReadyGeneration,
			),
			Revision: int64(record.Revision),
			ConfirmationHeight: optionToNullInt32(
				record.ConfirmationHeight,
			),
			ConfirmationBlockHash: optionHashToBytes(
				record.ConfirmationBlock,
			),
			CsvExpiryDelta:       record.CSVExpiryDelta,
			PolicyState:          int32(record.PolicyState),
			ConfirmationPkScript: record.ConfirmationPkScript,
			CreatedAt:            now,
			UpdatedAt:            now,
		},
	)
	if err != nil {
		return err
	}

	if err := q.DeleteBatchConsumedInputs(ctx, txid[:]); err != nil {
		return err
	}
	for _, in := range record.ConsumedInputs {
		err := q.InsertBatchConsumedInput(
			ctx, sqlc.InsertBatchConsumedInputParams{
				BatchTxid:     txid[:],
				InputHash:     in.Outpoint.Hash[:],
				InputIndex:    int32(in.Outpoint.Index),
				InputValue:    in.Value,
				InputPkScript: in.PkScript,
			},
		)
		if err != nil {
			return err
		}
	}

	if err := q.DeleteBatchDependentVTXOs(ctx, txid[:]); err != nil {
		return err
	}

	return insertDependentVTXOs(ctx, q, txid, record.DependentVTXOs)
}

// registrationMatches verifies that a repeated registration carries exactly
// the immutable evidence already stored for the batch.
func registrationMatches(existing, next *batchcanon.Record) error {
	switch {
	case existing.BatchTxID != next.BatchTxID:
		return fmt.Errorf("%w: txid changed",
			batchcanon.ErrRegistrationConflict)

	case !bytes.Equal(existing.BatchTx, next.BatchTx):
		return fmt.Errorf("%w: serialized transaction changed",
			batchcanon.ErrRegistrationConflict)

	case existing.BatchOutputIndex != next.BatchOutputIndex:
		return fmt.Errorf("%w: batch output index changed",
			batchcanon.ErrRegistrationConflict)

	case !bytes.Equal(
		existing.ConfirmationPkScript, next.ConfirmationPkScript,
	):
		return fmt.Errorf("%w: confirmation script changed",
			batchcanon.ErrRegistrationConflict)

	case existing.CSVExpiryDelta != next.CSVExpiryDelta:
		return fmt.Errorf("%w: csv expiry changed",
			batchcanon.ErrRegistrationConflict)

	case existing.PolicyState != next.PolicyState:
		return fmt.Errorf("%w: policy state changed",
			batchcanon.ErrRegistrationConflict)
	}

	if len(existing.ConsumedInputs) != len(next.ConsumedInputs) {
		return fmt.Errorf("%w: consumed-input count changed",
			batchcanon.ErrRegistrationConflict)
	}

	type inputEvidence struct {
		value    int64
		pkScript []byte
	}
	expected := make(
		map[wire.OutPoint]inputEvidence, len(existing.ConsumedInputs),
	)
	for _, in := range existing.ConsumedInputs {
		expected[in.Outpoint] = inputEvidence{
			value:    in.Value,
			pkScript: in.PkScript,
		}
	}
	for _, in := range next.ConsumedInputs {
		evidence, ok := expected[in.Outpoint]
		if !ok || evidence.value != in.Value ||
			!bytes.Equal(evidence.pkScript, in.PkScript) {
			return fmt.Errorf("%w: consumed input %s changed",
				batchcanon.ErrRegistrationConflict, in.Outpoint)
		}
	}

	return nil
}

// insertConsumerEdges adds immutable logical-consumer evidence inside the same
// transaction as registration. A repeat must match the expected business
// revision and complete creator lineage exactly.
func insertConsumerEdges(ctx context.Context, q BatchCanonicalityStore,
	consumerBatch chainhash.Hash, edges []batchcanon.ConsumerEdge,
	now int64) error {

	seenEdges := make(map[wire.OutPoint]struct{}, len(edges))
	for _, edge := range edges {
		if edge.ConsumerBatch != (chainhash.Hash{}) &&
			edge.ConsumerBatch != consumerBatch {
			return fmt.Errorf("%w: consumer batch changed",
				batchcanon.ErrRegistrationConflict)
		}
		if edge.ExpectedRevision == 0 || len(edge.CreatorLineage) == 0 {
			return fmt.Errorf("%w: incomplete consumer edge %s",
				batchcanon.ErrRegistrationConflict,
				edge.ConsumedVTXO)
		}
		if _, duplicate := seenEdges[edge.ConsumedVTXO]; duplicate {
			return fmt.Errorf("%w: duplicate consumer edge %s",
				batchcanon.ErrRegistrationConflict,
				edge.ConsumedVTXO)
		}
		seenEdges[edge.ConsumedVTXO] = struct{}{}

		consumedHash := edge.ConsumedVTXO.Hash[:]
		key := sqlc.GetProvisionalConsumerParams{
			ConsumedVtxoHash:  consumedHash,
			ConsumedVtxoIndex: int32(edge.ConsumedVTXO.Index),
			ConsumerBatchTxid: consumerBatch[:],
		}
		existingRevision, err := q.GetProvisionalConsumer(ctx, key)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = q.InsertProvisionalConsumer(
				ctx, sqlc.InsertProvisionalConsumerParams{
					ConsumedVtxoHash: consumedHash,
					ConsumedVtxoIndex: int32(
						edge.ConsumedVTXO.Index,
					),
					ConsumerBatchTxid: consumerBatch[:],
					ExpectedVtxoRevision: int64(
						edge.ExpectedRevision,
					),
					CreatedAt: now,
				},
			)
			if err != nil {
				return err
			}
			if err := insertCreatorLineage(
				ctx, q, consumerBatch, edge,
			); err != nil {
				return err
			}

		case err != nil:
			return err

		case uint64(existingRevision) != edge.ExpectedRevision:
			return fmt.Errorf("%w: consumer edge %s "+
				"revision changed",
				batchcanon.ErrRegistrationConflict,
				edge.ConsumedVTXO)

		default:
			if err := creatorLineageMatches(
				ctx, q, consumerBatch, edge,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

// insertCreatorLineage persists the normalized complete lineage for a new
// edge, rejecting duplicates before SQL's idempotent constraint can hide them.
func insertCreatorLineage(ctx context.Context, q BatchCanonicalityStore,
	consumerBatch chainhash.Hash, edge batchcanon.ConsumerEdge) error {

	seen := make(map[chainhash.Hash]struct{}, len(edge.CreatorLineage))
	for _, creator := range edge.CreatorLineage {
		if creator == (chainhash.Hash{}) {
			return fmt.Errorf("%w: zero creator lineage txid",
				batchcanon.ErrRegistrationConflict)
		}
		if _, duplicate := seen[creator]; duplicate {
			return fmt.Errorf("%w: duplicate creator "+
				"lineage txid %s",
				batchcanon.ErrRegistrationConflict, creator)
		}
		seen[creator] = struct{}{}
		if err := q.InsertConsumerCreatorLineage(
			ctx, sqlc.InsertConsumerCreatorLineageParams{
				ConsumedVtxoHash: edge.ConsumedVTXO.Hash[:],
				ConsumedVtxoIndex: int32(
					edge.ConsumedVTXO.Index,
				),
				ConsumerBatchTxid: consumerBatch[:],
				CreatorBatchTxid:  creator[:],
			},
		); err != nil {
			return err
		}
	}

	return nil
}

// creatorLineageMatches checks repeat registration against the exact durable
// set; neither omission nor additive ancestry is allowed.
func creatorLineageMatches(ctx context.Context, q BatchCanonicalityStore,
	consumerBatch chainhash.Hash, edge batchcanon.ConsumerEdge) error {

	rows, err := q.ListConsumerCreatorLineage(
		ctx, sqlc.ListConsumerCreatorLineageParams{
			ConsumedVtxoHash:  edge.ConsumedVTXO.Hash[:],
			ConsumedVtxoIndex: int32(edge.ConsumedVTXO.Index),
			ConsumerBatchTxid: consumerBatch[:],
		},
	)
	if err != nil {
		return err
	}
	if len(rows) != len(edge.CreatorLineage) {
		return fmt.Errorf("%w: consumer edge %s creator "+
			"lineage changed", batchcanon.ErrRegistrationConflict,
			edge.ConsumedVTXO)
	}

	want := make(map[chainhash.Hash]struct{}, len(edge.CreatorLineage))
	for _, creator := range edge.CreatorLineage {
		want[creator] = struct{}{}
	}
	for _, raw := range rows {
		creator, err := chainhash.NewHash(raw)
		if err != nil {
			return err
		}
		if _, ok := want[*creator]; !ok {
			return fmt.Errorf("%w: consumer edge %s creator "+
				"lineage changed",
				batchcanon.ErrRegistrationConflict,
				edge.ConsumedVTXO)
		}
	}

	return nil
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

		rows, err := q.RecordBatchInputConflict(
			ctx, sqlc.RecordBatchInputConflictParams{
				BatchTxid:     batchTxid[:],
				InputHash:     outpoint.Hash[:],
				InputIndex:    int32(outpoint.Index),
				Conflicting:   boolToInt32(conflicting),
				ConflictFinal: boolToInt32(conflictFinal),
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("batch input %s not found", outpoint)
		}

		return nil
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

// ListPendingConsumerEdges returns the complete durable restore evidence owned
// by one consumer batch.
func (s *BatchCanonicalityPersistenceStore) ListPendingConsumerEdges(
	ctx context.Context, consumerBatch chainhash.Hash) (
	[]batchcanon.ConsumerEdge, error) {

	var edges []batchcanon.ConsumerEdge

	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.ListProvisionalConsumersForBatch(
			ctx, consumerBatch[:],
		)
		if err != nil {
			return err
		}

		edges = make([]batchcanon.ConsumerEdge, 0, len(rows))
		for _, row := range rows {
			hash, err := chainhash.NewHash(row.ConsumedVtxoHash)
			if err != nil {
				return err
			}
			outpoint := wire.OutPoint{
				Hash:  *hash,
				Index: uint32(row.ConsumedVtxoIndex),
			}
			consumedIndex := row.ConsumedVtxoIndex
			lineageRows, err := q.ListConsumerCreatorLineage(
				ctx, sqlc.ListConsumerCreatorLineageParams{
					ConsumedVtxoHash:  row.ConsumedVtxoHash,
					ConsumedVtxoIndex: consumedIndex,
					ConsumerBatchTxid: consumerBatch[:],
				},
			)
			if err != nil {
				return err
			}
			lineage := make([]chainhash.Hash, 0, len(lineageRows))
			for _, raw := range lineageRows {
				ancestor, err := chainhash.NewHash(raw)
				if err != nil {
					return err
				}
				lineage = append(lineage, *ancestor)
			}
			edges = append(edges, batchcanon.ConsumerEdge{
				ConsumedVTXO:  outpoint,
				ConsumerBatch: consumerBatch,
				ExpectedRevision: uint64(
					row.ExpectedVtxoRevision,
				),
				CreatorLineage: lineage,
			})
		}

		return nil
	})

	return edges, err
}

// ListPendingConsumerBatchesByCreator returns the distinct consumer batches
// whose pending restore evidence names creatorBatch in its complete lineage.
func (s *BatchCanonicalityPersistenceStore) ListPendingConsumerBatchesByCreator(
	ctx context.Context, creatorBatch chainhash.Hash) ([]chainhash.Hash,
	error) {

	var consumers []chainhash.Hash
	err := s.db.ExecTx(ctx, ReadTxOption(), func(
		q BatchCanonicalityStore) error {

		rows, err := q.ListPendingConsumerBatchesByCreator(
			ctx, creatorBatch[:],
		)
		if err != nil {
			return err
		}

		consumers = make([]chainhash.Hash, 0, len(rows))
		for _, raw := range rows {
			consumer, err := chainhash.NewHash(raw)
			if err != nil {
				return err
			}
			consumers = append(consumers, *consumer)
		}

		return nil
	})

	return consumers, err
}

// ResolveConsumerEdge completes an invalid candidate without restoring, or
// atomically performs the exact ForfeitedBy CAS and edge completion.
func (s *BatchCanonicalityPersistenceStore) ResolveConsumerEdge(
	ctx context.Context, edge batchcanon.ConsumerEdge, restore bool) (
	batchcanon.ConsumerEdgeResolution, error) {

	resolution := batchcanon.ConsumerEdgeDeferred
	err := s.db.ExecTx(ctx, WriteTxOption(), func(
		q BatchCanonicalityStore) error {

		if restore {
			consumerBatch := edge.ConsumerBatch[:]
			rows, err := q.RestoreForfeitedVTXOForConsumer(
				ctx, sqlc.RestoreForfeitedVTXOForConsumerParams{
					OutpointHash: edge.ConsumedVTXO.Hash[:],
					OutpointIndex: int32(
						edge.ConsumedVTXO.Index,
					),
					BusinessRevision: int64(
						edge.ExpectedRevision,
					),
					ForfeitConsumerTxid: consumerBatch,
					LastUpdateTime: s.clock.
						Now().
						Unix(),
				},
			)
			if err != nil {
				return err
			}
			if rows == 0 {
				return nil
			}
			if rows != 1 {
				return fmt.Errorf("restore CAS changed "+
					"%d VTXOs", rows)
			}
		}

		rows, err := q.DeleteProvisionalConsumer(
			ctx, sqlc.DeleteProvisionalConsumerParams{
				ConsumedVtxoHash: edge.ConsumedVTXO.Hash[:],
				ConsumedVtxoIndex: int32(
					edge.ConsumedVTXO.Index,
				),
				ConsumerBatchTxid: edge.ConsumerBatch[:],
				ExpectedVtxoRevision: int64(
					edge.ExpectedRevision,
				),
			},
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			if restore {
				return fmt.Errorf("restored VTXO has no " +
					"exact consumer edge")
			}

			return nil
		}
		if rows != 1 {
			return fmt.Errorf("completed %d consumer edges", rows)
		}

		if restore {
			resolution = batchcanon.ConsumerEdgeRestored
		} else {
			resolution = batchcanon.ConsumerEdgeCompleted
		}

		return nil
	})

	return resolution, err
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

// BackfillFromVTXOs derives a fail-closed placeholder for every distinct batch
// (commitment) txid present in the VTXO store that does not already have a
// record, so an upgrading node knows which historical evidence is missing. It
// is idempotent: batches that already have a record are left untouched, so a
// re-run never clobbers state the manager has since advanced. It returns the
// number of placeholder records created.
//
// The provisional/finalized State is an informational migration hint derived
// from stored height and the supplied policy depth. It never opens admission:
// the placeholder remains incomplete and not Ready until authenticated
// serialized transaction, output, and full prevout evidence replaces it in a
// fresh generation. The CSV-relative expiry delta is recovered as
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

			registrationStage := int32(
				batchcanon.RegistrationReconciling,
			)
			params := sqlc.UpsertBatchCanonicalityParams{
				BatchTxid:             txid[:],
				State:                 int32(state),
				ObservationGeneration: 1,
				ReadyGeneration:       sql.NullInt64{},
				Revision:              0,
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
			}
			params.RegistrationStage = registrationStage
			err = q.UpsertBatchCanonicality(ctx, params)
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
			Value:         in.InputValue,
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
		BatchTxID:        *txid,
		BatchTx:          row.BatchTx,
		BatchOutputIndex: uint32(row.BatchOutputIndex.Int32),
		RegistrationStage: batchcanon.RegistrationStage(
			row.RegistrationStage,
		),
		ObservationGeneration: uint64(row.ObservationGeneration),
		ReadyGeneration: nullInt64ToOptionUint64(
			row.ReadyGeneration,
		),
		Revision:             uint64(row.Revision),
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

// batchOutputIndexToNullInt32 keeps legacy/backfilled records without a
// serialized transaction explicitly incomplete. Complete registrations bind
// an index, including zero, to their transaction bytes.
func batchOutputIndexToNullInt32(batchTx []byte, index uint32) sql.NullInt32 {
	if len(batchTx) == 0 {
		return sql.NullInt32{}
	}

	return sql.NullInt32{
		Int32: int32(index),
		Valid: true,
	}
}

// optionUint64ToNullInt64 maps a generation to its SQL representation.
func optionUint64ToNullInt64(o fn.Option[uint64]) sql.NullInt64 {
	if o.IsNone() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{
		Int64: int64(o.UnwrapOr(0)),
		Valid: true,
	}
}

// nullInt64ToOptionUint64 maps a nullable generation back to an option.
func nullInt64ToOptionUint64(n sql.NullInt64) fn.Option[uint64] {
	if !n.Valid {
		return fn.None[uint64]()
	}

	return fn.Some(uint64(n.Int64))
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
