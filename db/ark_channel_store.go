package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

const initialArkChannelRevision = int64(1)

// ArkChannelStoreDB persists the narrow Ark-to-lnd coordination state.
type ArkChannelStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewArkChannelStore creates a revisioned Ark channel store.
func NewArkChannelStore(store *Store, clk clock.Clock) *ArkChannelStoreDB {
	baseDB := store.BaseDB()
	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &ArkChannelStoreDB{
		TransactionExecutor: txExec,
		clock:               clk,
	}
}

// Create inserts a channel at revision one without replacing existing terms.
func (s *ArkChannelStoreDB) Create(ctx context.Context,
	snapshot arkchannel.Snapshot) (arkchannel.Record, error) {

	if _, err := arkchannel.RestoreState(snapshot); err != nil {
		return arkchannel.Record{}, err
	}
	now := s.clock.Now().Unix()
	params, err := insertArkChannelParams(snapshot, now)
	if err != nil {
		return arkchannel.Record{}, err
	}

	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		var err error
		rows, err = q.InsertArkChannel(ctx, params)

		return err
	})
	if err != nil {
		return arkchannel.Record{}, err
	}
	if rows != 1 {
		return arkchannel.Record{}, arkchannel.ErrConflict
	}

	return arkchannel.Record{
		Snapshot: snapshot.Clone(),
		Revision: uint64(initialArkChannelRevision),
	}, nil
}

// Get loads one channel by stable ID.
func (s *ArkChannelStoreDB) Get(ctx context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	var row sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetArkChannel(ctx, id[:])

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	if err != nil {
		return arkchannel.Record{}, err
	}

	return arkChannelRecordFromRow(row)
}

// GetByPendingChannelID loads one channel by lnd's funding correlation ID.
func (s *ArkChannelStoreDB) GetByPendingChannelID(ctx context.Context,
	pendingID [32]byte) (arkchannel.Record, error) {

	var row sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetArkChannelByPendingID(ctx, pendingID[:])

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	if err != nil {
		return arkchannel.Record{}, err
	}

	return arkChannelRecordFromRow(row)
}

// GetByChannelPoint loads one channel by lnd's durable funding outpoint.
func (s *ArkChannelStoreDB) GetByChannelPoint(ctx context.Context,
	channelPoint wire.OutPoint) (arkchannel.Record, error) {

	var row sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetArkChannelByChannelPoint(
			ctx, sqlc.GetArkChannelByChannelPointParams{
				ChannelPointTxid: channelPoint.Hash[:],
				ChannelPointIndex: sql.NullInt64{
					Int64: int64(channelPoint.Index),
					Valid: true,
				},
			},
		)

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	if err != nil {
		return arkchannel.Record{}, err
	}

	return arkChannelRecordFromRow(row)
}

// ListNonTerminal loads channels that need recovery or observation.
func (s *ArkChannelStoreDB) ListNonTerminal(ctx context.Context) (
	[]arkchannel.Record, error) {

	var rows []sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListNonTerminalArkChannels(ctx)

		return err
	})
	if err != nil {
		return nil, err
	}

	records := make([]arkchannel.Record, 0, len(rows))
	for _, row := range rows {
		record, err := arkChannelRecordFromRow(row)
		if err != nil {
			return nil, err
		}

		records = append(records, record)
	}

	return records, nil
}

// CompareAndSwap advances mutable state from one expected revision.
func (s *ArkChannelStoreDB) CompareAndSwap(ctx context.Context,
	id arkchannel.ID, expected uint64, snapshot arkchannel.Snapshot) (
	arkchannel.Record, error) {

	if snapshot.Terms.ID != id {
		return arkchannel.Record{}, fmt.Errorf("snapshot channel ID " +
			"mismatch")
	}
	if expected == 0 || expected >= math.MaxInt64 {
		return arkchannel.Record{}, fmt.Errorf("invalid expected "+
			"revision %d", expected)
	}
	if _, err := arkchannel.RestoreState(snapshot); err != nil {
		return arkchannel.Record{}, err
	}

	params, err := compareAndSwapArkChannelParams(
		snapshot, int64(expected), s.clock.Now().Unix(),
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		var err error
		rows, err = q.CompareAndSwapArkChannel(ctx, params)

		return err
	})
	if err != nil {
		return arkchannel.Record{}, err
	}
	if rows != 1 {
		return arkchannel.Record{}, arkchannel.ErrConflict
	}

	return arkchannel.Record{
		Snapshot: snapshot.Clone(),
		Revision: expected + 1,
	}, nil
}

// arkChannelMutableFields is the shared SQL representation of mutable state.
type arkChannelMutableFields struct {
	phase             int32
	sourceTxID        []byte
	sourceIndex       sql.NullInt64
	sourceAmount      sql.NullInt64
	roundID           sql.NullString
	commitmentTxID    []byte
	backingTx         []byte
	channelPointTxID  []byte
	channelPointIndex sql.NullInt64
	clientFinalized   bool
	hubFinalized      bool
	roundCommitted    bool
	roundConfirmed    bool
	backingPublished  bool
	failure           sql.NullString
}

// mutableArkChannelFields converts optional snapshot fields for sqlc.
func mutableArkChannelFields(snapshot arkchannel.Snapshot) (
	arkChannelMutableFields, error) {

	fields := arkChannelMutableFields{
		phase:            int32(snapshot.Phase),
		clientFinalized:  snapshot.ClientFinalized,
		hubFinalized:     snapshot.HubFinalized,
		roundCommitted:   snapshot.RoundCommitted,
		roundConfirmed:   snapshot.RoundConfirmed,
		backingPublished: snapshot.BackingPublished,
		failure: sql.NullString{
			String: snapshot.Failure,
			Valid:  snapshot.Failure != "",
		},
	}
	if snapshot.Source != nil {
		fields.sourceTxID = slices.Clone(
			snapshot.Source.OutPoint.Hash[:],
		)
		fields.sourceIndex = sql.NullInt64{
			Int64: int64(snapshot.Source.OutPoint.Index),
			Valid: true,
		}
		fields.sourceAmount = sql.NullInt64{
			Int64: int64(snapshot.Source.Amount),
			Valid: true,
		}
		fields.roundID = sql.NullString{
			String: snapshot.Source.RoundID,
			Valid:  true,
		}
		fields.commitmentTxID = slices.Clone(
			snapshot.Source.CommitmentTxID[:],
		)
	}
	if snapshot.Backing != nil {
		fields.backingTx = slices.Clone(snapshot.Backing.Transaction)
		fields.channelPointTxID = slices.Clone(
			snapshot.Backing.ChannelPoint.Hash[:],
		)
		fields.channelPointIndex = sql.NullInt64{
			Int64: int64(snapshot.Backing.ChannelPoint.Index),
			Valid: true,
		}
	}

	return fields, nil
}

// insertArkChannelParams builds a complete immutable insert.
func insertArkChannelParams(snapshot arkchannel.Snapshot,
	now int64) (sqlc.InsertArkChannelParams, error) {

	fields, err := mutableArkChannelFields(snapshot)
	if err != nil {
		return sqlc.InsertArkChannelParams{}, err
	}
	terms := snapshot.Terms

	return sqlc.InsertArkChannelParams{
		ChannelID:         slices.Clone(terms.ID[:]),
		Kind:              int32(terms.Kind),
		Funder:            int32(terms.Funder),
		PendingChannelID:  slices.Clone(terms.PendingChannelID[:]),
		ReservedScid:      encodeSCID(terms.ReservedSCID),
		Capacity:          int64(terms.Capacity),
		ClientNodeKey:     slices.Clone(terms.ClientNodeKey[:]),
		HubNodeKey:        slices.Clone(terms.HubNodeKey[:]),
		PaymentHash:       slices.Clone(terms.PaymentHash[:]),
		ClientArkKey:      slices.Clone(terms.VTXO.ClientArkKey[:]),
		HubArkKey:         slices.Clone(terms.VTXO.HubArkKey[:]),
		ArkOperatorKey:    slices.Clone(terms.VTXO.ArkOperatorKey[:]),
		ClientChannelKey:  slices.Clone(terms.VTXO.ClientChannelKey[:]),
		HubChannelKey:     slices.Clone(terms.VTXO.HubChannelKey[:]),
		FunderKey:         slices.Clone(terms.VTXO.FunderKey[:]),
		ChannelDelay:      int64(terms.VTXO.ChannelDelay),
		FunderDelay:       int64(terms.VTXO.FunderDelay),
		MinExitDelay:      int64(terms.VTXO.MinExitDelay),
		Phase:             fields.phase,
		SourceTxid:        fields.sourceTxID,
		SourceIndex:       fields.sourceIndex,
		SourceAmount:      fields.sourceAmount,
		RoundID:           fields.roundID,
		CommitmentTxid:    fields.commitmentTxID,
		BackingTx:         fields.backingTx,
		ChannelPointTxid:  fields.channelPointTxID,
		ChannelPointIndex: fields.channelPointIndex,
		ClientFinalized:   fields.clientFinalized,
		HubFinalized:      fields.hubFinalized,
		RoundCommitted:    fields.roundCommitted,
		RoundConfirmed:    fields.roundConfirmed,
		BackingPublished:  fields.backingPublished,
		Failure:           fields.failure,
		Revision:          initialArkChannelRevision,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// compareAndSwapArkChannelParams builds a mutable revision update.
func compareAndSwapArkChannelParams(snapshot arkchannel.Snapshot, revision,
	now int64) (sqlc.CompareAndSwapArkChannelParams, error) {

	fields, err := mutableArkChannelFields(snapshot)
	if err != nil {
		return sqlc.CompareAndSwapArkChannelParams{}, err
	}

	return sqlc.CompareAndSwapArkChannelParams{
		ChannelID:         slices.Clone(snapshot.Terms.ID[:]),
		Revision:          revision,
		Phase:             fields.phase,
		SourceTxid:        fields.sourceTxID,
		SourceIndex:       fields.sourceIndex,
		SourceAmount:      fields.sourceAmount,
		RoundID:           fields.roundID,
		CommitmentTxid:    fields.commitmentTxID,
		BackingTx:         fields.backingTx,
		ChannelPointTxid:  fields.channelPointTxID,
		ChannelPointIndex: fields.channelPointIndex,
		ClientFinalized:   fields.clientFinalized,
		HubFinalized:      fields.hubFinalized,
		RoundCommitted:    fields.roundCommitted,
		RoundConfirmed:    fields.roundConfirmed,
		BackingPublished:  fields.backingPublished,
		Failure:           fields.failure,
		UpdatedAt:         now,
	}, nil
}

// arkChannelRecordFromRow validates one SQL row before exposing it.
func arkChannelRecordFromRow(row sqlc.ArkChannel) (arkchannel.Record, error) {
	var terms arkchannel.Terms
	if err := copyFixed(
		terms.ID[:], row.ChannelID, "channel ID",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.PendingChannelID[:], row.PendingChannelID,
		"pending channel ID",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.ClientNodeKey[:], row.ClientNodeKey, "client node key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.HubNodeKey[:], row.HubNodeKey, "hub node key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.PaymentHash[:], row.PaymentHash, "payment hash",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.ClientArkKey[:], row.ClientArkKey, "client Ark key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.HubArkKey[:], row.HubArkKey, "hub Ark key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.ArkOperatorKey[:], row.ArkOperatorKey,
		"Ark operator key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.ClientChannelKey[:], row.ClientChannelKey,
		"client channel key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.HubChannelKey[:], row.HubChannelKey,
		"hub channel key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	if err := copyFixed(
		terms.VTXO.FunderKey[:], row.FunderKey, "funder key",
	); err != nil {
		return arkchannel.Record{}, err
	}
	channelDelay, err := decodeDelay(row.ChannelDelay, "channel delay")
	if err != nil {
		return arkchannel.Record{}, err
	}
	funderDelay, err := decodeDelay(row.FunderDelay, "funder delay")
	if err != nil {
		return arkchannel.Record{}, err
	}
	minExitDelay, err := decodeDelay(
		row.MinExitDelay, "minimum exit delay",
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	terms.VTXO.ChannelDelay = channelDelay
	terms.VTXO.FunderDelay = funderDelay
	terms.VTXO.MinExitDelay = minExitDelay
	reservedSCID, err := decodeSCID(row.ReservedScid)
	if err != nil {
		return arkchannel.Record{}, err
	}
	terms.Kind = arkchannel.Kind(row.Kind)
	terms.Funder = arkchannel.Party(row.Funder)
	terms.ReservedSCID = reservedSCID
	terms.Capacity = btcutil.Amount(row.Capacity)

	snapshot := arkchannel.Snapshot{
		Terms:            terms,
		Phase:            arkchannel.Phase(row.Phase),
		ClientFinalized:  row.ClientFinalized,
		HubFinalized:     row.HubFinalized,
		RoundCommitted:   row.RoundCommitted,
		RoundConfirmed:   row.RoundConfirmed,
		BackingPublished: row.BackingPublished,
		Failure:          row.Failure.String,
	}
	source, err := arkChannelSourceFromRow(row, terms)
	if err != nil {
		return arkchannel.Record{}, err
	}
	snapshot.Source = source
	backing, err := arkChannelBackingFromRow(row)
	if err != nil {
		return arkchannel.Record{}, err
	}
	snapshot.Backing = backing

	if row.Revision <= 0 {
		return arkchannel.Record{}, fmt.Errorf("invalid channel "+
			"revision %d", row.Revision)
	}
	if _, err := arkchannel.RestoreState(snapshot); err != nil {
		return arkchannel.Record{}, fmt.Errorf("restore Ark "+
			"channel: %w", err)
	}

	return arkchannel.Record{
		Snapshot: snapshot,
		Revision: uint64(row.Revision),
	}, nil
}

// arkChannelSourceFromRow decodes the optional source group.
func arkChannelSourceFromRow(row sqlc.ArkChannel,
	terms arkchannel.Terms) (*arkchannel.VTXOBinding, error) {

	if len(row.SourceTxid) == 0 {
		return nil, nil
	}
	sourceHash, err := chainhash.NewHash(row.SourceTxid)
	if err != nil {
		return nil, fmt.Errorf("decode source txid: %w", err)
	}
	commitmentTxID, err := chainhash.NewHash(row.CommitmentTxid)
	if err != nil {
		return nil, fmt.Errorf("decode commitment txid: %w", err)
	}
	sourceIndex, err := decodeIndex(row.SourceIndex, "source index")
	if err != nil {
		return nil, err
	}
	if !row.SourceAmount.Valid {
		return nil, fmt.Errorf("source amount is missing")
	}
	if !row.RoundID.Valid {
		return nil, fmt.Errorf("source round ID is missing")
	}
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		return nil, err
	}

	return &arkchannel.VTXOBinding{
		OutPoint: wire.OutPoint{
			Hash:  *sourceHash,
			Index: sourceIndex,
		},
		Amount:         btcutil.Amount(row.SourceAmount.Int64),
		RoundID:        row.RoundID.String,
		CommitmentTxID: *commitmentTxID,
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}, nil
}

// arkChannelBackingFromRow decodes the optional signed-backing group.
func arkChannelBackingFromRow(row sqlc.ArkChannel) (*arkchannel.Backing,
	error) {

	if len(row.BackingTx) == 0 {
		return nil, nil
	}
	txID, err := chainhash.NewHash(row.ChannelPointTxid)
	if err != nil {
		return nil, fmt.Errorf("decode channel point txid: %w", err)
	}
	index, err := decodeIndex(row.ChannelPointIndex, "channel point index")
	if err != nil {
		return nil, err
	}

	return &arkchannel.Backing{
		Transaction: slices.Clone(row.BackingTx),
		ChannelPoint: wire.OutPoint{
			Hash:  *txID,
			Index: index,
		},
	}, nil
}

// copyFixed rejects corrupt fixed-width SQL blobs.
func copyFixed(dst, src []byte, field string) error {
	if len(src) != len(dst) {
		return fmt.Errorf("%s has %d bytes, expected %d", field,
			len(src), len(dst))
	}

	copy(dst, src)

	return nil
}

// encodeSCID preserves the full unsigned short-channel-ID domain.
func encodeSCID(scid uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], scid)

	return encoded[:]
}

// decodeSCID decodes the fixed-width unsigned short channel ID.
func decodeSCID(raw []byte) (uint64, error) {
	if len(raw) != 8 {
		return 0, fmt.Errorf("reserved SCID has %d bytes, expected 8",
			len(raw))
	}

	return binary.BigEndian.Uint64(raw), nil
}

// decodeIndex validates a nullable SQL index before narrowing it.
func decodeIndex(index sql.NullInt64, field string) (uint32, error) {
	if !index.Valid {
		return 0, fmt.Errorf("%s is missing", field)
	}
	if index.Int64 < 0 || index.Int64 > math.MaxUint32 {
		return 0, fmt.Errorf("%s %d is out of range", field,
			index.Int64)
	}

	return uint32(index.Int64), nil
}

// decodeDelay rejects corrupt SQL values before narrowing to BIP-68 fields.
func decodeDelay(delay int64, field string) (uint32, error) {
	if delay < 0 || delay > math.MaxUint32 {
		return 0, fmt.Errorf("%s %d is out of range", field, delay)
	}

	return uint32(delay), nil
}

var _ arkchannel.Store = (*ArkChannelStoreDB)(nil)
