package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/unroller"
	"github.com/lightningnetwork/lnd/clock"
)

// UnrollPersistenceStore implements unroller.UnrollStore using SQLite
// for durable persistence of in-progress VTXO tree unrolls. It
// delegates VTXO lookups to VTXOPersistenceStore and stores
// unroll-specific state (status, level, etc.) in the `unrolls` table
// via sqlc-generated queries.
//
// Complex derived fields (LevelOrder, BroadcastTxids, ConfirmedTxids)
// are NOT persisted. They are re-derived from the VTXO tree on load
// via extractLevelOrder. This is safe because:
//   - LevelOrder is deterministic given the same tree
//   - BroadcastTxids/ConfirmedTxids are transient tracking state that
//     the unroller rebuilds by re-broadcasting from CurrentLevel
type UnrollPersistenceStore struct {
	db    BatchedRoundStore
	clock clock.Clock

	// vtxoStore is the real VTXO persistence store used to look
	// up VTXO descriptors (including tree paths) by outpoint.
	vtxoStore *VTXOPersistenceStore
}

// NewUnrollPersistenceStore creates a new SQLite-backed unroll store
// that delegates VTXO lookups to the given persistence store.
func NewUnrollPersistenceStore(
	db BatchedRoundStore, vtxoStore *VTXOPersistenceStore,
	clk clock.Clock,
) *UnrollPersistenceStore {

	return &UnrollPersistenceStore{
		db:        db,
		clock:     clk,
		vtxoStore: vtxoStore,
	}
}

// GetVTXO retrieves a VTXO by outpoint from the real persistence
// store and converts it to round.ClientVTXO for the unroller.
func (s *UnrollPersistenceStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*round.ClientVTXO, error) {

	desc, err := s.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("vtxo not found: %v: %w",
			outpoint, err)
	}

	return &round.ClientVTXO{
		Outpoint:    desc.Outpoint,
		Amount:      desc.Amount,
		PkScript:    desc.PkScript,
		Expiry:      desc.RelativeExpiry,
		ClientKey:   desc.ClientKey,
		OperatorKey: desc.OperatorKey,
		TreePath:    desc.TreePath,
	}, nil
}

// SaveUnrollState creates a new unroll tracking record in the
// database.
func (s *UnrollPersistenceStore) SaveUnrollState(ctx context.Context,
	state *unroller.UnrollState) error {

	nowUnix := s.clock.Now().Unix()

	var errorMsg sql.NullString
	if state.Error != nil {
		errorMsg = sql.NullString{
			String: state.Error.Error(),
			Valid:  true,
		}
	}

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		return q.InsertUnroll(ctx, sqlc.InsertUnrollParams{
			VtxoOutpointHash:    state.VTXOOutpoint.Hash[:],
			VtxoOutpointIndex:   int32(state.VTXOOutpoint.Index),
			Status:              int32(state.Status),
			CurrentLevel:        int32(state.CurrentLevel),
			LeafConfirmHeight:   state.LeafConfirmHeight,
			ErrorMsg:            errorMsg,
			RetryCount:          int32(state.RetryCount),
			LastBroadcastHeight: state.LastBroadcastHeight,
			CurrentFeeRate:      state.CurrentFeeRate,
			CreatedAt:           nowUnix,
			UpdatedAt:           nowUnix,
		})
	})
}

// UpdateUnrollState updates an existing unroll record in the
// database.
func (s *UnrollPersistenceStore) UpdateUnrollState(ctx context.Context,
	state *unroller.UnrollState) error {

	nowUnix := s.clock.Now().Unix()

	var errorMsg sql.NullString
	if state.Error != nil {
		errorMsg = sql.NullString{
			String: state.Error.Error(),
			Valid:  true,
		}
	}

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		return q.UpdateUnroll(ctx, sqlc.UpdateUnrollParams{
			VtxoOutpointHash:    state.VTXOOutpoint.Hash[:],
			VtxoOutpointIndex:   int32(state.VTXOOutpoint.Index),
			Status:              int32(state.Status),
			CurrentLevel:        int32(state.CurrentLevel),
			LeafConfirmHeight:   state.LeafConfirmHeight,
			ErrorMsg:            errorMsg,
			RetryCount:          int32(state.RetryCount),
			LastBroadcastHeight: state.LastBroadcastHeight,
			CurrentFeeRate:      state.CurrentFeeRate,
			UpdatedAt:           nowUnix,
		})
	})
}

// GetUnrollState retrieves unroll state by VTXO outpoint. The
// returned UnrollState has LevelOrder, BroadcastTxids, and
// ConfirmedTxids unset — callers must re-derive these from the
// VTXO tree if needed.
func (s *UnrollPersistenceStore) GetUnrollState(ctx context.Context,
	vtxoOutpoint wire.OutPoint) (*unroller.UnrollState, error) {

	readTxOpts := ReadTxOption()

	var result *unroller.UnrollState

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		row, err := q.GetUnroll(ctx, sqlc.GetUnrollParams{
			VtxoOutpointHash:  vtxoOutpoint.Hash[:],
			VtxoOutpointIndex: int32(vtxoOutpoint.Index),
		})
		if err != nil {
			return fmt.Errorf("get unroll: %w", err)
		}

		result = unrollRowToState(row)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ListActiveUnrolls returns all in-progress unrolls (not complete or
// failed). The returned states have LevelOrder, BroadcastTxids, and
// ConfirmedTxids unset.
func (s *UnrollPersistenceStore) ListActiveUnrolls(
	ctx context.Context) ([]*unroller.UnrollState, error) {

	readTxOpts := ReadTxOption()

	var result []*unroller.UnrollState

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListActiveUnrolls(ctx)
		if err != nil {
			return fmt.Errorf("list active unrolls: %w", err)
		}

		for _, row := range rows {
			result = append(result, unrollRowToState(row))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// DeleteUnrollState removes a completed unroll record from the
// database.
func (s *UnrollPersistenceStore) DeleteUnrollState(ctx context.Context,
	vtxoOutpoint wire.OutPoint) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		return q.DeleteUnroll(ctx, sqlc.DeleteUnrollParams{
			VtxoOutpointHash:  vtxoOutpoint.Hash[:],
			VtxoOutpointIndex: int32(vtxoOutpoint.Index),
		})
	})
}

// unrollRowToState converts a sqlc.Unroll row into an
// unroller.UnrollState. Maps and slices are initialized to empty to
// prevent nil pointer dereferences in the unroller.
func unrollRowToState(row sqlc.Unroll) *unroller.UnrollState {
	var hash chainhash.Hash
	copy(hash[:], row.VtxoOutpointHash)

	outpoint := wire.OutPoint{
		Hash:  hash,
		Index: uint32(row.VtxoOutpointIndex),
	}

	state := &unroller.UnrollState{
		VTXOOutpoint:        outpoint,
		Status:              unroller.UnrollStatus(row.Status),
		CurrentLevel:        int(row.CurrentLevel),
		LeafConfirmHeight:   row.LeafConfirmHeight,
		RetryCount:          int(row.RetryCount),
		LastBroadcastHeight: row.LastBroadcastHeight,
		CurrentFeeRate:      row.CurrentFeeRate,
		BroadcastTxids:      make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	if row.ErrorMsg.Valid {
		state.Error = fmt.Errorf("%s", row.ErrorMsg.String)
	}

	return state
}

// Compile-time check that UnrollPersistenceStore implements
// unroller.UnrollStore.
var _ unroller.UnrollStore = (*UnrollPersistenceStore)(nil)
