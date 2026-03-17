package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/rounds"
)

// SQLCStore adapts *sqlc.Queries to the indexer Store interface,
// translating between sqlc-generated types and indexer domain types.
// It embeds a TransactionExecutor for atomic multi-query operations.
type SQLCStore struct {
	q  *sqlc.Queries
	tx *db.TransactionExecutor[*sqlc.Queries]
}

// NewSQLCStore creates a new Store adapter wrapping the given queries.
// The optional BatchedQuerier enables transactional reads via
// ExecReadTx; pass nil to use non-transactional queries only.
func NewSQLCStore(q *sqlc.Queries,
	opts ...SQLCStoreOption) *SQLCStore {

	s := &SQLCStore{q: q}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// SQLCStoreOption is a functional option for SQLCStore.
type SQLCStoreOption func(*SQLCStore)

// WithBatchedQuerier enables transactional reads by embedding a
// TransactionExecutor backed by the provided BatchedQuerier.
func WithBatchedQuerier(dbq db.BatchedQuerier) SQLCStoreOption {
	return func(s *SQLCStore) {
		s.tx = db.NewTransactionExecutor[*sqlc.Queries](
			dbq,
			func(tx *sql.Tx) *sqlc.Queries {
				return sqlc.NewWithBackend(tx, dbq.Backend())
			},
			nil,
		)
	}
}

// ExecReadTx runs fn inside a read-only database transaction,
// providing a transactional SQLCStore to the callback. All queries
// issued through the callback's store see a consistent snapshot.
func (s *SQLCStore) ExecReadTx(ctx context.Context,
	fn func(Store) error) error {

	if s.tx == nil {
		return fn(s)
	}

	return s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			txStore := &SQLCStore{q: q}
			return fn(txStore)
		},
	)
}

// Compile-time check that *SQLCStore satisfies the Store interface.
var _ Store = (*SQLCStore)(nil)

// ListActiveReceiveScriptsByPrincipal implements
// ScriptRegistrationReader.
func (s *SQLCStore) ListActiveReceiveScriptsByPrincipal(
	ctx context.Context, principal string,
	now time.Time) ([]ReceiveScript, error) {

	rows, err := s.q.ListActiveIndexerReceiveScriptsByPrincipal(
		ctx,
		sqlc.ListActiveIndexerReceiveScriptsByPrincipalParams{
			PrincipalMailboxID: principal,
			ExpiresAtUnixS:     now.Unix(),
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]ReceiveScript, len(rows))
	for i, r := range rows {
		out[i] = receiveScriptFromSQLC(r)
	}

	return out, nil
}

// UpsertReceiveScript implements Store.
func (s *SQLCStore) UpsertReceiveScript(ctx context.Context,
	principal string, pkScript []byte,
	expiresAt time.Time, label string,
	updatedAt time.Time) error {

	return s.q.UpsertIndexerReceiveScript(
		ctx,
		sqlc.UpsertIndexerReceiveScriptParams{
			PrincipalMailboxID: principal,
			PkScript:           pkScript,
			ExpiresAtUnixS:     expiresAt.Unix(),
			Label:              label,
			UpdatedAt:          updatedAt.Unix(),
		},
	)
}

// DeleteReceiveScript implements Store.
func (s *SQLCStore) DeleteReceiveScript(ctx context.Context,
	principal string, pkScript []byte) (int64, error) {

	return s.q.DeleteIndexerReceiveScript(
		ctx,
		sqlc.DeleteIndexerReceiveScriptParams{
			PrincipalMailboxID: principal,
			PkScript:           pkScript,
		},
	)
}

// ListOORRecipientEventsAfterWithSession implements Store.
func (s *SQLCStore) ListOORRecipientEventsAfterWithSession(
	ctx context.Context, recipientPkScript []byte,
	afterEventID int64, limit int32,
) ([]OORRecipientEventWithSession, error) {

	rows, err := s.q.ListOORRecipientEventsAfterWithSession(
		ctx,
		sqlc.ListOORRecipientEventsAfterWithSessionParams{
			RecipientPkScript: recipientPkScript,
			EventID:           afterEventID,
			Limit:             limit,
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]OORRecipientEventWithSession, len(rows))
	for i, r := range rows {
		out[i] = OORRecipientEventWithSession{
			RecipientPkScript: r.RecipientPkScript,
			EventID:           r.EventID,
			SessionID:         r.SessionID,
			OutputIndex:       r.OutputIndex,
			Value:             r.Value,
			ArkPsbt:           r.ArkPsbt,
		}
	}

	return out, nil
}

// GetOORSessionCheckpoints returns all checkpoint PSBTs for a
// session by querying the oor_checkpoints table joined with
// oor_sessions.
func (s *SQLCStore) GetOORSessionCheckpoints(ctx context.Context,
	sessionID []byte) ([]OORSessionCheckpoint, error) {

	rows, err := s.q.GetOORSessionCheckpoints(
		ctx, sessionID,
	)
	if err != nil {
		return nil, err
	}

	out := make([]OORSessionCheckpoint, len(rows))
	for i, r := range rows {
		out[i] = OORSessionCheckpoint{
			CheckpointIndex: r.CheckpointIndex,
			CheckpointPsbt:  r.CheckpointPsbt,
		}
	}

	return out, nil
}

// ListVTXOsByPkScripts implements VTXOReader.
func (s *SQLCStore) ListVTXOsByPkScripts(ctx context.Context,
	pkScripts [][]byte) ([]VTXORow, error) {

	var rows []sqlc.Vtxo
	var err error

	switch s.q.Backend() {
	case sqlc.BackendTypeSqlite:
		rows, err = s.q.ListVTXOsByPkScriptsSqlite(
			ctx, pkScripts,
		)

	case sqlc.BackendTypePostgres:
		rows, err = s.q.ListVTXOsByPkScriptsPostgres(
			ctx, pkScripts,
		)

	default:
		return nil, fmt.Errorf(
			"unknown backend: %v", s.q.Backend(),
		)
	}
	if err != nil {
		return nil, err
	}

	out := make([]VTXORow, len(rows))
	for i, r := range rows {
		v, err := vtxoRowFromSQLC(r)
		if err != nil {
			return nil, err
		}

		out[i] = v
	}

	return out, nil
}

// GetVTXO implements VTXOReader.
func (s *SQLCStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (VTXORow, error) {

	row, err := s.q.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  outpoint.Hash[:],
		OutpointIndex: int32(outpoint.Index),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VTXORow{}, ErrNotFound
		}

		return VTXORow{}, err
	}

	return vtxoRowFromSQLC(row)
}

// GetRound implements VTXOReader.
func (s *SQLCStore) GetRound(ctx context.Context,
	roundID rounds.RoundID) (RoundRow, error) {

	r, err := s.q.GetRound(ctx, roundID[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoundRow{}, ErrNotFound
		}

		return RoundRow{}, err
	}

	return roundRowFromSQLC(r)
}

// ListRoundsByIDs implements VTXOReader.
func (s *SQLCStore) ListRoundsByIDs(ctx context.Context,
	roundIDs []rounds.RoundID) ([]RoundRow, error) {

	rawIDs := make([][]byte, len(roundIDs))
	for i, id := range roundIDs {
		idCopy := id
		rawIDs[i] = idCopy[:]
	}

	var rows []sqlc.Round
	var err error

	switch s.q.Backend() {
	case sqlc.BackendTypeSqlite:
		rows, err = s.q.ListRoundsByIDsSqlite(ctx, rawIDs)

	case sqlc.BackendTypePostgres:
		rows, err = s.q.ListRoundsByIDsPostgres(ctx, rawIDs)

	default:
		return nil, fmt.Errorf(
			"unknown backend: %v", s.q.Backend(),
		)
	}
	if err != nil {
		return nil, err
	}

	out := make([]RoundRow, len(rows))
	for i, r := range rows {
		rr, err := roundRowFromSQLC(r)
		if err != nil {
			return nil, err
		}

		out[i] = rr
	}

	return out, nil
}

// LoadVTXOTree implements TreeLoader.
//
// This method encapsulates the full tree deserialization pipeline:
// round lookup, final-tx parsing, sweep-key derivation, and recursive
// tree reconstruction.
func (s *SQLCStore) LoadVTXOTree(ctx context.Context,
	roundID rounds.RoundID,
	batchOutputIndex int) (*tree.Tree, error) {

	roundRow, err := s.q.GetRound(ctx, roundID[:])
	if err != nil {
		return nil, fmt.Errorf("get round: %w", err)
	}

	if len(roundRow.FinalTx) == 0 {
		return nil, fmt.Errorf("missing final tx")
	}

	finalTx := &wire.MsgTx{}
	if err := finalTx.Deserialize(
		bytes.NewReader(roundRow.FinalTx),
	); err != nil {
		return nil, fmt.Errorf("deserialize final tx: %w", err)
	}

	sweepKey, err := btcec.ParsePubKey(roundRow.SweepKey)
	if err != nil {
		return nil, fmt.Errorf("parse sweep key: %w", err)
	}

	sweepTapLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
		sweepKey, uint32(roundRow.CsvDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("compute sweep tapscript: %w", err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	commitmentTxid := finalTx.TxHash()
	batchOutpoint := wire.OutPoint{
		Hash:  commitmentTxid,
		Index: uint32(batchOutputIndex),
	}

	if batchOutputIndex < 0 ||
		batchOutputIndex >= len(finalTx.TxOut) {

		return nil, fmt.Errorf("batch output index out of range")
	}
	batchOutput := finalTx.TxOut[batchOutputIndex]

	vtxoTree, err := db.DeserializeTreeRecursive(
		ctx, s.q, roundID, batchOutputIndex,
		batchOutpoint, batchOutput, sweepTapRoot[:],
	)
	if err != nil {
		return nil, fmt.Errorf("deserialize tree: %w", err)
	}

	return vtxoTree, nil
}

// GetOORRecipientEventBySessionOutput implements OORReader.
func (s *SQLCStore) GetOORRecipientEventBySessionOutput(
	ctx context.Context, recipientPkScript, sessionID []byte,
	outputIndex int32) (OORRecipientEvent, error) {

	r, err := s.q.GetOORRecipientEventBySessionOutput(
		ctx,
		sqlc.GetOORRecipientEventBySessionOutputParams{
			RecipientPkScript: recipientPkScript,
			SessionID:         sessionID,
			OutputIndex:       outputIndex,
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OORRecipientEvent{}, ErrNotFound
		}

		return OORRecipientEvent{}, err
	}

	return OORRecipientEvent{
		EventID:           r.EventID,
		RecipientPkScript: r.RecipientPkScript,
		OutputIndex:       r.OutputIndex,
		Value:             r.Value,
	}, nil
}

// GetOORSession implements OORReader.
func (s *SQLCStore) GetOORSession(ctx context.Context,
	sessionID []byte) (OORSession, error) {

	r, err := s.q.GetOORSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OORSession{}, ErrNotFound
		}

		return OORSession{}, err
	}

	return OORSession{
		ID:      r.ID,
		ArkPsbt: append([]byte(nil), r.ArkPsbt...),
	}, nil
}

// ListOORCheckpoints implements OORReader.
func (s *SQLCStore) ListOORCheckpoints(ctx context.Context,
	sessionDBID int32) ([]OORCheckpoint, error) {

	rows, err := s.q.ListOORCheckpoints(ctx, sessionDBID)
	if err != nil {
		return nil, err
	}

	out := make([]OORCheckpoint, len(rows))
	for i, r := range rows {
		pkt, err := psbt.NewFromRawBytes(
			bytes.NewReader(r.CheckpointPsbt), false,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"parse checkpoint psbt %d: %w", i, err,
			)
		}

		out[i] = OORCheckpoint{Psbt: pkt}
	}

	return out, nil
}

// InsertOORRecipientEvent implements Store.
func (s *SQLCStore) InsertOORRecipientEvent(ctx context.Context,
	recipientPkScript []byte, eventID int64,
	sessionDBID, outputIndex int32,
	value int64,
	createdAt time.Time) (int64, error) {

	// The SQL uses ON CONFLICT DO NOTHING, so a PK collision on
	// (recipient_pk_script, event_id) silently returns
	// rowsAffected=0 with err=nil instead of raising a constraint
	// error. We detect this case and surface ErrUniqueViolation so
	// the CAS retry loop in insertRecipientEvent can increment the
	// candidate event ID.
	rowsAffected, err := s.q.InsertOORRecipientEvent(
		ctx,
		sqlc.InsertOORRecipientEventParams{
			RecipientPkScript: recipientPkScript,
			EventID:           eventID,
			SessionDbID:       sessionDBID,
			OutputIndex:       outputIndex,
			Value:             value,
			CreatedAt:         createdAt.Unix(),
		},
	)
	if err != nil {
		mapped := db.MapSQLError(err)
		var uniqueErr *db.ErrSQLUniqueConstraintViolation
		if errors.As(mapped, &uniqueErr) {
			return 0, ErrUniqueViolation
		}

		return 0, mapped
	}

	if rowsAffected == 0 {
		return 0, ErrUniqueViolation
	}

	return eventID, nil
}

// GetMaxOORRecipientEventID implements Store.
func (s *SQLCStore) GetMaxOORRecipientEventID(ctx context.Context,
	recipientPkScript []byte) (int64, error) {

	return s.q.GetMaxOORRecipientEventID(ctx, recipientPkScript)
}

// ListActiveReceivePrincipalsByScript implements Store.
func (s *SQLCStore) ListActiveReceivePrincipalsByScript(
	ctx context.Context, pkScript []byte,
	now time.Time) ([]ReceiveScript, error) {

	rows, err := s.q.ListActiveIndexerReceivePrincipalsByScript(
		ctx,
		sqlc.ListActiveIndexerReceivePrincipalsByScriptParams{
			PkScript:       pkScript,
			ExpiresAtUnixS: now.Unix(),
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]ReceiveScript, len(rows))
	for i, r := range rows {
		out[i] = receiveScriptFromSQLC(r)
	}

	return out, nil
}

// ListVTXOEventsAfterByScripts implements Store.
func (s *SQLCStore) ListVTXOEventsAfterByScripts(ctx context.Context,
	afterEventID int64, pkScripts [][]byte,
	limit int32) ([]VTXOEvent, error) {

	var rows []sqlc.IndexerVtxoEvent
	var err error

	switch s.q.Backend() {
	case sqlc.BackendTypeSqlite:
		rows, err = s.q.ListIndexerVTXOEventsAfterByScriptsSqlite(
			ctx,
			sqlc.ListIndexerVTXOEventsAfterByScriptsSqliteParams{
				EventID:   afterEventID,
				PkScripts: pkScripts,
				Limit:     limit,
			},
		)

	case sqlc.BackendTypePostgres:
		rows, err = s.q.ListIndexerVTXOEventsAfterByScriptsPostgres(
			ctx,
			sqlc.ListIndexerVTXOEventsAfterByScriptsPostgresParams{
				PkScripts:    pkScripts,
				AfterEventID: afterEventID,
				QueryLimit:   limit,
			},
		)

	default:
		return nil, fmt.Errorf(
			"unknown backend: %v", s.q.Backend(),
		)
	}
	if err != nil {
		return nil, err
	}

	out := make([]VTXOEvent, len(rows))
	for i, r := range rows {
		var op wire.OutPoint
		copy(op.Hash[:], r.OutpointHash)
		op.Index = uint32(r.OutpointIndex)

		out[i] = VTXOEvent{
			Outpoint:  op,
			EventID:   r.EventID,
			EventType: r.EventType,
			Status:    r.Status,
			CreatedAt: time.Unix(r.CreatedAt, 0),
		}
	}

	return out, nil
}

// InsertVTXOEvent implements Store.
func (s *SQLCStore) InsertVTXOEvent(ctx context.Context,
	pkScript []byte, eventType string,
	outpoint wire.OutPoint,
	vtxoStatus string,
	createdAt time.Time) (int64, error) {

	return s.q.InsertIndexerVTXOEvent(
		ctx,
		sqlc.InsertIndexerVTXOEventParams{
			PkScript:      pkScript,
			EventType:     eventType,
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			Status:        vtxoStatus,
			CreatedAt:     createdAt.Unix(),
		},
	)
}

// receiveScriptFromSQLC converts a sqlc IndexerReceiveScript row to an
// indexer ReceiveScript domain type.
func receiveScriptFromSQLC(r sqlc.IndexerReceiveScript) ReceiveScript {
	return ReceiveScript{
		PrincipalMailboxID: r.PrincipalMailboxID,
		PkScript:           append([]byte(nil), r.PkScript...),
		ExpiresAt:          time.Unix(r.ExpiresAtUnixS, 0),
		Label:              r.Label,
	}
}

// vtxoRowFromSQLC converts a sqlc Vtxo row to an indexer VTXORow
// domain type.
func vtxoRowFromSQLC(r sqlc.Vtxo) (VTXORow, error) {
	var op wire.OutPoint
	if len(r.OutpointHash) != 32 {
		return VTXORow{}, fmt.Errorf(
			"unexpected outpoint hash length: %d",
			len(r.OutpointHash),
		)
	}
	copy(op.Hash[:], r.OutpointHash)
	op.Index = uint32(r.OutpointIndex)

	row := VTXORow{
		Outpoint: op,
		Amount:   r.Amount,
		PkScript: append([]byte(nil), r.PkScript...),
		Status:   r.Status,
	}

	if r.BatchOutputIndex.Valid {
		idx := r.BatchOutputIndex.Int32
		row.BatchOutputIndex = &idx
	}

	if len(r.RoundID) == 16 {
		var id rounds.RoundID
		copy(id[:], r.RoundID)
		row.RoundID = &id
	}

	return row, nil
}

// roundRowFromSQLC converts a sqlc Round row to an indexer RoundRow
// domain type.
func roundRowFromSQLC(r sqlc.Round) (RoundRow, error) {
	if len(r.RoundID) != 16 {
		return RoundRow{}, fmt.Errorf(
			"unexpected round_id length: %d",
			len(r.RoundID),
		)
	}

	var roundID rounds.RoundID
	copy(roundID[:], r.RoundID)

	var commitTxid chainhash.Hash
	if r.CommitmentTxid != "" {
		decoded, err := chainhash.NewHashFromStr(r.CommitmentTxid)
		if err != nil {
			return RoundRow{}, fmt.Errorf(
				"decode commitment_txid: %w", err,
			)
		}
		commitTxid = *decoded
	}

	roundRow := RoundRow{
		RoundID:        roundID,
		CommitmentTxid: commitTxid,
		CsvDelay:       r.CsvDelay,
	}

	if r.ConfirmationHeight.Valid {
		height := r.ConfirmationHeight.Int32
		roundRow.ConfirmationHeight = &height
	}

	return roundRow, nil
}
