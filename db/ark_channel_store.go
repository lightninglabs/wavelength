package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

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
	peer  *[33]byte
}

// ForPeer restricts operations on the shared database to one authenticated
// client. A scoped view borrows the database; it never owns its lifetime.
func (s *ArkChannelStoreDB) ForPeer(peer [33]byte) *ArkChannelStoreDB {
	return &ArkChannelStoreDB{
		TransactionExecutor: s.TransactionExecutor,
		clock:               s.clock,
		peer:                &peer,
	}
}

// RecoveryPeers lists owners with native, unroll or refresh-defense work.
func (s *ArkChannelStoreDB) RecoveryPeers(ctx context.Context) ([][33]byte,
	error) {

	var peers [][33]byte
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		keys, err := q.ListArkRecoveryPeers(ctx)
		if err != nil {
			return err
		}
		peers = nil
		for _, key := range keys {
			var peer [33]byte
			if err := copyFixed(
				peer[:], key, "client node key",
			); err != nil {
				return err
			}
			if s.peer == nil || peer == *s.peer {
				peers = append(peers, peer)
			}
		}

		return nil
	})

	return peers, err
}

// readChannel conceals absent and foreign records behind the same result,
// regardless of which unique key was used to find the channel.
func (s *ArkChannelStoreDB) readChannel(ctx context.Context,
	lookup func(*sqlc.Queries) (sqlc.ArkChannel, error)) (arkchannel.Record,
	error) {

	var row sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		row, err = lookup(q)

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	if err != nil {
		return arkchannel.Record{}, err
	}
	if s.peer != nil && !slices.Equal(row.ClientNodeKey, s.peer[:]) {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}

	return arkChannelRecordFromRow(row)
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

	if s.peer != nil && *s.peer != snapshot.Terms.ClientNodeKey {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	if _, err := arkchannel.RestoreState(snapshot); err != nil {
		return arkchannel.Record{}, err
	}
	nowTime := time.Unix(s.clock.Now().Unix(), 0).UTC()
	now := nowTime.Unix()
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

	record := arkchannel.Record{
		Snapshot:  snapshot.Clone(),
		Revision:  uint64(initialArkChannelRevision),
		CreatedAt: nowTime, UpdatedAt: nowTime,
	}
	if snapshot.OORPreparationStarted || snapshot.Source != nil {
		record.PrePONRStartedAt = nowTime
	}

	return record, nil
}

// Get loads one channel by stable ID.
func (s *ArkChannelStoreDB) Get(ctx context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	return s.readChannel(
		ctx,
		func(q *sqlc.Queries) (sqlc.ArkChannel, error) {
			return q.GetArkChannel(ctx, id[:])
		},
	)
}

// GetByPendingChannelID loads one channel by lnd's funding correlation ID.
func (s *ArkChannelStoreDB) GetByPendingChannelID(ctx context.Context,
	pendingID [32]byte) (arkchannel.Record, error) {

	return s.readChannel(
		ctx,
		func(q *sqlc.Queries) (sqlc.ArkChannel, error) {
			return q.GetArkChannelByPendingID(ctx, pendingID[:])
		},
	)
}

// GetBySCID resolves an alias without loading unrelated signed transactions.
func (s *ArkChannelStoreDB) GetBySCID(ctx context.Context, scid uint64) (
	arkchannel.Record, error) {

	return s.readChannel(
		ctx,
		func(q *sqlc.Queries) (sqlc.ArkChannel, error) {
			return q.GetArkChannelBySCID(ctx, encodeSCID(scid))
		},
	)
}

// GetByChannelPoint loads one channel by lnd's durable funding outpoint.
func (s *ArkChannelStoreDB) GetByChannelPoint(ctx context.Context,
	channelPoint wire.OutPoint) (arkchannel.Record, error) {

	params := sqlc.GetArkChannelByChannelPointParams{
		ChannelPointTxid: channelPoint.Hash[:],
		ChannelPointIndex: sql.NullInt64{
			Int64: int64(channelPoint.Index), Valid: true,
		},
	}

	return s.readChannel(
		ctx,
		func(q *sqlc.Queries) (sqlc.ArkChannel, error) {
			return q.GetArkChannelByChannelPoint(ctx, params)
		},
	)
}

// ListNonTerminal loads channels that need recovery or observation. This
// includes cooperatively closed channels whose replacement VTXOs retain a
// source-ancestry defense obligation.
func (s *ArkChannelStoreDB) ListNonTerminal(ctx context.Context) (
	[]arkchannel.Record, error) {

	var rows []sqlc.ArkChannel
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		if s.peer == nil {
			rows, err = q.ListNonTerminalArkChannels(ctx)
		} else {
			rows, err = q.ListPeerArkChannels(ctx, s.peer[:])
		}

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

// BindFundingTxID claims the canonical backing at the same revision whose
// terms, source and phase authorized signing. It cannot replace a prior claim.
func (s *ArkChannelStoreDB) BindFundingTxID(ctx context.Context,
	terms arkchannel.Terms, revision uint64, txid chainhash.Hash) error {

	if s.peer != nil && *s.peer != terms.ClientNodeKey {
		return arkchannel.ErrNotFound
	}
	if revision == 0 || revision >= math.MaxInt64 {
		return fmt.Errorf("invalid funding authorization revision %d",
			revision)
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.AuthorizeArkFundingBacking(ctx,
			sqlc.AuthorizeArkFundingBackingParams{
				Txid: txid[:], UpdatedAt: s.clock.Now().Unix(),
				ChannelID: terms.ID[:], Revision: int64(
					revision,
				),
				OwnerKey: terms.ClientNodeKey[:],
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return arkchannel.ErrConflict
		}

		return nil
	})
}

// RecordFundingSignature commits the winning signature before its caller can
// release it. A concurrent cancellation invalidates the observed revision.
func (s *ArkChannelStoreDB) RecordFundingSignature(ctx context.Context,
	terms arkchannel.Terms, revision uint64, txid chainhash.Hash,
	sig []byte) error {

	if s.peer != nil && *s.peer != terms.ClientNodeKey {
		return arkchannel.ErrNotFound
	}
	if revision == 0 || revision >= math.MaxInt64 {
		return fmt.Errorf("invalid funding signature revision %d",
			revision)
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.RecordArkFundingSignature(ctx,
			sqlc.RecordArkFundingSignatureParams{
				Signature: sig,
				Txid:      txid[:],
				UpdatedAt: s.clock.Now().Unix(),
				ChannelID: terms.ID[:],
				Revision:  int64(revision),
				OwnerKey:  terms.ClientNodeKey[:],
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return arkchannel.ErrConflict
		}

		return nil
	})
}

// CompareAndSwap advances mutable state from one expected revision.
func (s *ArkChannelStoreDB) CompareAndSwap(ctx context.Context,
	id arkchannel.ID, expected uint64, snapshot arkchannel.Snapshot) (
	arkchannel.Record, error) {

	if s.peer != nil && *s.peer != snapshot.Terms.ClientNodeKey {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
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
	var (
		rows int64
		row  sqlc.ArkChannel
	)
	err = s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		var err error
		rows, err = q.CompareAndSwapArkChannel(ctx, params)
		if err != nil || rows != 1 {
			return err
		}
		row, err = q.GetArkChannel(ctx, id[:])

		return err
	})
	if err != nil {
		return arkchannel.Record{}, err
	}
	if rows != 1 {
		return arkchannel.Record{}, arkchannel.ErrConflict
	}

	return arkChannelRecordFromRow(row)
}

// arkChannelMutableFields is the shared SQL representation of mutable state.
type arkChannelMutableFields struct {
	phase                    int32
	oorSessionID             []byte
	sourceIndex              sql.NullInt64
	sourceAmount             sql.NullInt64
	sourceArkTx              []byte
	backingTx                []byte
	channelPointTxID         []byte
	channelPointIndex        sql.NullInt64
	clientFinalized          bool
	hubFinalized             bool
	oorFinalized             bool
	oorAborted               bool
	clientRecoveryReady      bool
	hubRecoveryReady         bool
	sourceSpentOutpointID    []byte
	sourceSpentOutpointIndex sql.NullInt64
	sourceSpendingTxID       []byte
	backingPublished         bool
	closeInitiator           sql.NullInt32
	closeClientScript        []byte
	closeHubScript           []byte
	closeFeeRate             sql.NullInt64
	cooperativeCloseTx       []byte
	cooperativeCloseTxID     []byte
	closeCommitmentHeight    sql.NullInt64
	closeClientBalance       sql.NullInt64
	closeHubBalance          sql.NullInt64
	failure                  sql.NullString
}

// mutableArkChannelFields converts optional snapshot fields for sqlc.
func mutableArkChannelFields(snapshot arkchannel.Snapshot) (
	arkChannelMutableFields, error) {

	fields := arkChannelMutableFields{
		phase:               int32(snapshot.Phase),
		clientFinalized:     snapshot.ClientFinalized,
		hubFinalized:        snapshot.HubFinalized,
		oorFinalized:        snapshot.OORFinalized,
		oorAborted:          snapshot.OORAborted,
		clientRecoveryReady: snapshot.ClientRecoveryReady,
		hubRecoveryReady:    snapshot.HubRecoveryReady,
		backingPublished:    snapshot.BackingPublished,
		failure: sql.NullString{
			String: snapshot.Failure,
			Valid:  snapshot.Failure != "",
		},
	}
	if snapshot.SourceConflict != nil {
		fields.sourceSpentOutpointID = slices.Clone(
			snapshot.SourceConflict.OutPoint.Hash[:],
		)
		fields.sourceSpentOutpointIndex = sql.NullInt64{
			Int64: int64(snapshot.SourceConflict.OutPoint.Index),
			Valid: true,
		}
		fields.sourceSpendingTxID = slices.Clone(
			snapshot.SourceConflict.SpendingTxID[:],
		)
	}
	if snapshot.Source != nil {
		fields.oorSessionID = slices.Clone(
			snapshot.Source.OORSessionID[:],
		)
		fields.sourceIndex = sql.NullInt64{
			Int64: int64(snapshot.Source.OutPoint.Index),
			Valid: true,
		}
		fields.sourceAmount = sql.NullInt64{
			Int64: int64(snapshot.Source.Amount),
			Valid: true,
		}
		fields.sourceArkTx = slices.Clone(
			snapshot.Source.ArkTransaction,
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
	if snapshot.CooperativeCloseRequest != nil {
		request := snapshot.CooperativeCloseRequest
		fields.closeInitiator = sql.NullInt32{
			Int32: int32(request.Initiator),
			Valid: true,
		}
		fields.closeClientScript = slices.Clone(
			request.ClientDeliveryScript,
		)
		fields.closeHubScript = slices.Clone(request.HubDeliveryScript)
		// The column predates in-Ark OOR close semantics. Keep a zero
		// sentinel so existing databases retain their group constraint.
		fields.closeFeeRate = sql.NullInt64{Valid: true}
	}
	if snapshot.CooperativeClose != nil {
		settlement := snapshot.CooperativeClose
		if settlement.Proposal.CommitmentHeight > math.MaxInt64 {
			return arkChannelMutableFields{}, fmt.Errorf(
				"cooperative close commitment height is out " +
					"of range")
		}
		fields.cooperativeCloseTx = slices.Clone(settlement.Transaction)
		fields.cooperativeCloseTxID = slices.Clone(settlement.TxID[:])
		fields.closeCommitmentHeight = sql.NullInt64{
			Int64: int64(settlement.Proposal.CommitmentHeight),
			Valid: true,
		}
		fields.closeClientBalance = sql.NullInt64{
			Int64: int64(settlement.Proposal.ClientBalance),
			Valid: true,
		}
		fields.closeHubBalance = sql.NullInt64{
			Int64: int64(settlement.Proposal.HubBalance),
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
		ChannelID:        slices.Clone(terms.ID[:]),
		PendingChannelID: slices.Clone(terms.PendingChannelID[:]),
		ReservedScid:     encodeSCID(terms.ReservedSCID),
		Capacity:         int64(terms.Capacity),
		ClientNodeKey:    slices.Clone(terms.ClientNodeKey[:]),
		HubNodeKey:       slices.Clone(terms.HubNodeKey[:]),
		ClientArkKey:     slices.Clone(terms.VTXO.ClientArkKey[:]),
		HubArkKey:        slices.Clone(terms.VTXO.HubArkKey[:]),
		ArkOperatorKey: slices.Clone(
			terms.VTXO.ArkOperatorKey[:],
		),
		ClientChannelKey: slices.Clone(
			terms.VTXO.ClientChannelKey[:],
		),
		HubChannelKey: slices.Clone(
			terms.VTXO.HubChannelKey[:],
		),
		FunderKey:                slices.Clone(terms.VTXO.FunderKey[:]),
		ChannelDelay:             int64(terms.VTXO.ChannelDelay),
		FunderDelay:              int64(terms.VTXO.FunderDelay),
		MinExitDelay:             int64(terms.VTXO.MinExitDelay),
		Phase:                    fields.phase,
		OorSessionID:             fields.oorSessionID,
		SourceIndex:              fields.sourceIndex,
		SourceAmount:             fields.sourceAmount,
		SourceArkTx:              fields.sourceArkTx,
		BackingTx:                fields.backingTx,
		ChannelPointTxid:         fields.channelPointTxID,
		ChannelPointIndex:        fields.channelPointIndex,
		ClientFinalized:          fields.clientFinalized,
		HubFinalized:             fields.hubFinalized,
		OorFinalized:             fields.oorFinalized,
		OorAborted:               fields.oorAborted,
		ClientRecoveryReady:      fields.clientRecoveryReady,
		HubRecoveryReady:         fields.hubRecoveryReady,
		SourceSpentOutpointTxid:  fields.sourceSpentOutpointID,
		SourceSpentOutpointIndex: fields.sourceSpentOutpointIndex,
		SourceSpendingTxid:       fields.sourceSpendingTxID,
		BackingPublished:         fields.backingPublished,
		CloseInitiator:           fields.closeInitiator,
		CloseClientScript:        fields.closeClientScript,
		CloseHubScript:           fields.closeHubScript,
		CloseFeeRateSatPerKw:     fields.closeFeeRate,
		CooperativeCloseTx:       fields.cooperativeCloseTx,
		CooperativeCloseTxid:     fields.cooperativeCloseTxID,
		CloseCommitmentHeight:    fields.closeCommitmentHeight,
		CloseClientBalance:       fields.closeClientBalance,
		CloseHubBalance:          fields.closeHubBalance,
		Failure:                  fields.failure,
		Revision:                 initialArkChannelRevision,
		CreatedAt:                now,
		UpdatedAt:                now,
		PrePonrStartedAt: arkChannelPrePONRStartedAt(
			snapshot, now,
		),
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
		ChannelID:                slices.Clone(snapshot.Terms.ID[:]),
		Revision:                 revision,
		OwnerKey:                 snapshot.Terms.ClientNodeKey[:],
		Phase:                    fields.phase,
		OorSessionID:             fields.oorSessionID,
		SourceIndex:              fields.sourceIndex,
		SourceAmount:             fields.sourceAmount,
		SourceArkTx:              fields.sourceArkTx,
		BackingTx:                fields.backingTx,
		ChannelPointTxid:         fields.channelPointTxID,
		ChannelPointIndex:        fields.channelPointIndex,
		ClientFinalized:          fields.clientFinalized,
		HubFinalized:             fields.hubFinalized,
		OorFinalized:             fields.oorFinalized,
		OorAborted:               fields.oorAborted,
		ClientRecoveryReady:      fields.clientRecoveryReady,
		HubRecoveryReady:         fields.hubRecoveryReady,
		SourceSpentOutpointTxid:  fields.sourceSpentOutpointID,
		SourceSpentOutpointIndex: fields.sourceSpentOutpointIndex,
		SourceSpendingTxid:       fields.sourceSpendingTxID,
		BackingPublished:         fields.backingPublished,
		CloseInitiator:           fields.closeInitiator,
		CloseClientScript:        fields.closeClientScript,
		CloseHubScript:           fields.closeHubScript,
		CloseFeeRateSatPerKw:     fields.closeFeeRate,
		CooperativeCloseTx:       fields.cooperativeCloseTx,
		CooperativeCloseTxid:     fields.cooperativeCloseTxID,
		CloseCommitmentHeight:    fields.closeCommitmentHeight,
		CloseClientBalance:       fields.closeClientBalance,
		CloseHubBalance:          fields.closeHubBalance,
		Failure:                  fields.failure,
		UpdatedAt:                now,
		PrePonrStartedAt: arkChannelPrePONRStartedAt(
			snapshot, now,
		),
	}, nil
}

// arkChannelPrePONRStartedAt returns the first timestamp candidate only after
// preparation is durably armed. SQL preserves any earlier timestamp.
func arkChannelPrePONRStartedAt(snapshot arkchannel.Snapshot,
	now int64) sql.NullInt64 {

	started := snapshot.OORPreparationStarted || snapshot.Source != nil

	return sql.NullInt64{Int64: now, Valid: started}
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
	terms.Kind = arkchannel.KindPromotion
	terms.Funder = arkchannel.PartyClient
	terms.ReservedSCID = reservedSCID
	terms.Capacity = btcutil.Amount(row.Capacity)

	snapshot := arkchannel.Snapshot{
		Terms:                 terms,
		Phase:                 arkchannel.Phase(row.Phase),
		OORPreparationStarted: row.PrePonrStartedAt.Valid,
		ClientFinalized:       row.ClientFinalized,
		HubFinalized:          row.HubFinalized,
		OORFinalized:          row.OorFinalized,
		OORAborted:            row.OorAborted,
		ClientRecoveryReady:   row.ClientRecoveryReady,
		HubRecoveryReady:      row.HubRecoveryReady,
		BackingPublished:      row.BackingPublished,
		Failure:               row.Failure.String,
	}
	conflict, err := arkChannelSourceConflictFromRow(row)
	if err != nil {
		return arkchannel.Record{}, err
	}
	snapshot.SourceConflict = conflict
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
	closeRequest, err := arkChannelCloseRequestFromRow(row)
	if err != nil {
		return arkchannel.Record{}, err
	}
	snapshot.CooperativeCloseRequest = closeRequest
	cooperativeClose, err := arkChannelCooperativeCloseFromRow(
		row, terms, source, closeRequest,
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	snapshot.CooperativeClose = cooperativeClose

	if row.Revision <= 0 {
		return arkchannel.Record{}, fmt.Errorf("invalid channel "+
			"revision %d", row.Revision)
	}
	if _, err := arkchannel.RestoreState(snapshot); err != nil {
		return arkchannel.Record{}, fmt.Errorf("restore Ark "+
			"channel: %w", err)
	}

	var authorization *chainhash.Hash
	if row.AuthorizedBackingTxid != nil {
		authorization, err = chainhash.NewHash(
			row.AuthorizedBackingTxid,
		)
		if err != nil {
			return arkchannel.Record{}, err
		}
	}

	return arkchannel.Record{
		AuthorizedBackingTxID: authorization,
		FundingSignature:      row.FundingSignature,
		Snapshot:              snapshot,
		Revision:              uint64(row.Revision),
		CreatedAt:             time.Unix(row.CreatedAt, 0).UTC(),
		UpdatedAt:             time.Unix(row.UpdatedAt, 0).UTC(),
		PrePONRStartedAt: nullableUnixTime(
			row.PrePonrStartedAt,
		),
	}, nil
}

// nullableUnixTime decodes optional persisted lifecycle metadata.
func nullableUnixTime(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}

	return time.Unix(value.Int64, 0).UTC()
}

// arkChannelSourceConflictFromRow restores the all-or-none source-spend tuple.
func arkChannelSourceConflictFromRow(row sqlc.ArkChannel) (
	*arkchannel.SourceConflict, error) {

	present := len(row.SourceSpentOutpointTxid) != 0 ||
		row.SourceSpentOutpointIndex.Valid ||
		len(row.SourceSpendingTxid) != 0
	if !present {
		return nil, nil
	}
	if len(row.SourceSpentOutpointTxid) != chainhash.HashSize ||
		!row.SourceSpentOutpointIndex.Valid ||
		row.SourceSpentOutpointIndex.Int64 < 0 ||
		row.SourceSpentOutpointIndex.Int64 > math.MaxUint32 ||
		len(row.SourceSpendingTxid) != chainhash.HashSize {
		return nil, fmt.Errorf("incomplete Ark channel source conflict")
	}
	outpointTxID, err := chainhash.NewHash(row.SourceSpentOutpointTxid)
	if err != nil {
		return nil, err
	}
	spendingTxID, err := chainhash.NewHash(row.SourceSpendingTxid)
	if err != nil {
		return nil, err
	}

	return &arkchannel.SourceConflict{
		OutPoint: wire.OutPoint{
			Hash:  *outpointTxID,
			Index: uint32(row.SourceSpentOutpointIndex.Int64),
		},
		SpendingTxID: *spendingTxID,
	}, nil
}

// arkChannelSourceFromRow decodes the optional source group.
func arkChannelSourceFromRow(row sqlc.ArkChannel,
	terms arkchannel.Terms) (*arkchannel.VTXOBinding, error) {

	if len(row.OorSessionID) == 0 {
		return nil, nil
	}
	var oorSessionID [32]byte
	if err := copyFixed(
		oorSessionID[:], row.OorSessionID, "OOR session ID",
	); err != nil {
		return nil, err
	}
	sourceIndex, err := decodeIndex(row.SourceIndex, "source index")
	if err != nil {
		return nil, err
	}
	if !row.SourceAmount.Valid {
		return nil, fmt.Errorf("source amount is missing")
	}
	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		return nil, err
	}

	return &arkchannel.VTXOBinding{
		OORSessionID: oorSessionID,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash(oorSessionID),
			Index: sourceIndex,
		},
		Amount:         btcutil.Amount(row.SourceAmount.Int64),
		ArkTransaction: slices.Clone(row.SourceArkTx),
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

// arkChannelCloseRequestFromRow decodes the optional cooperative-close request
// group.
func arkChannelCloseRequestFromRow(row sqlc.ArkChannel) (
	*arkchannel.CooperativeCloseRequest, error) {

	if !row.CloseInitiator.Valid {
		if len(row.CloseClientScript) != 0 ||
			len(row.CloseHubScript) != 0 ||
			row.CloseFeeRateSatPerKw.Valid {
			return nil, fmt.Errorf("cooperative close request is " +
				"incomplete")
		}

		return nil, nil
	}
	if len(row.CloseClientScript) == 0 || len(row.CloseHubScript) == 0 ||
		!row.CloseFeeRateSatPerKw.Valid {
		return nil, fmt.Errorf("cooperative close request is " +
			"incomplete")
	}
	request := &arkchannel.CooperativeCloseRequest{
		Initiator: arkchannel.Party(row.CloseInitiator.Int32),
		ClientDeliveryScript: slices.Clone(
			row.CloseClientScript,
		),
		HubDeliveryScript: slices.Clone(row.CloseHubScript),
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}

	return request, nil
}

// arkChannelCooperativeCloseFromRow rebuilds the canonical unsigned proposal
// from compact SQL facts and attaches the persisted signed witness.
func arkChannelCooperativeCloseFromRow(row sqlc.ArkChannel,
	terms arkchannel.Terms, source *arkchannel.VTXOBinding,
	request *arkchannel.CooperativeCloseRequest) (
	*arkchannel.CooperativeClose, error) {

	if len(row.CooperativeCloseTx) == 0 {
		if len(row.CooperativeCloseTxid) != 0 ||
			row.CloseCommitmentHeight.Valid ||
			row.CloseClientBalance.Valid ||
			row.CloseHubBalance.Valid {
			return nil, fmt.Errorf("cooperative close artifact " +
				"is incomplete")
		}

		return nil, nil
	}
	if source == nil || request == nil ||
		len(row.CooperativeCloseTxid) == 0 ||
		!row.CloseCommitmentHeight.Valid ||
		!row.CloseClientBalance.Valid || !row.CloseHubBalance.Valid {
		return nil, fmt.Errorf("cooperative close artifact is " +
			"incomplete")
	}
	if row.CloseCommitmentHeight.Int64 < 0 ||
		row.CloseClientBalance.Int64 < 0 ||
		row.CloseHubBalance.Int64 < 0 {
		return nil, fmt.Errorf("cooperative close artifact has " +
			"negative values")
	}
	txID, err := chainhash.NewHash(row.CooperativeCloseTxid)
	if err != nil {
		return nil, fmt.Errorf("decode cooperative close txid: %w", err)
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, *source, *request,
		btcutil.Amount(row.CloseClientBalance.Int64),
		btcutil.Amount(row.CloseHubBalance.Int64),
		uint64(row.CloseCommitmentHeight.Int64),
	)
	if err != nil {
		return nil, err
	}
	settlement := &arkchannel.CooperativeClose{
		Proposal:    template.Proposal(),
		Transaction: slices.Clone(row.CooperativeCloseTx),
		TxID:        *txID,
	}
	if err := settlement.Validate(terms, *source, *request); err != nil {
		return nil, err
	}

	return settlement, nil
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
