package db

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/unroller"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestUnrollStore creates an UnrollPersistenceStore backed by a
// temporary SQLite database for testing. The vtxoStore is nil since
// GetVTXO is not exercised in these unit tests.
func newTestUnrollStore(t *testing.T) *UnrollPersistenceStore {
	t.Helper()

	sqliteStore := NewTestSqliteDB(t)
	clk := clock.NewDefaultClock()

	queries := sqlc.NewSqlite(sqliteStore.DB)
	store := NewStore(
		sqliteStore.DB, queries, sqlc.BackendTypeSqlite,
		btclog.Disabled,
	)

	return store.NewUnrollStore(nil, clk)
}

// randomOutpoint creates a deterministic outpoint from a seed byte
// for test reproducibility.
func randomOutpoint(seed byte) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = seed

	return wire.OutPoint{Hash: hash, Index: uint32(seed)}
}

func TestUnrollStoreSaveAndGet(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(1)
	state := &unroller.UnrollState{
		VTXOOutpoint:      outpoint,
		Status:            unroller.UnrollStatusPending,
		CurrentLevel:      0,
		LeafConfirmHeight: 0,
		RetryCount:        0,
		BroadcastTxids:    make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	// Save.
	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	// Get.
	got, err := store.GetUnrollState(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, outpoint, got.VTXOOutpoint)
	require.Equal(t, unroller.UnrollStatusPending, got.Status)
	require.Equal(t, 0, got.CurrentLevel)
	require.Equal(t, int32(0), got.LeafConfirmHeight)
	require.Nil(t, got.Error)
}

func TestUnrollStoreUpdate(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(2)
	state := &unroller.UnrollState{
		VTXOOutpoint:   outpoint,
		Status:         unroller.UnrollStatusPending,
		CurrentLevel:   0,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	// Update to broadcasting at level 1.
	state.Status = unroller.UnrollStatusBroadcasting
	state.CurrentLevel = 1
	err = store.UpdateUnrollState(ctx, state)
	require.NoError(t, err)

	got, err := store.GetUnrollState(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, unroller.UnrollStatusBroadcasting, got.Status)
	require.Equal(t, 1, got.CurrentLevel)
}

func TestUnrollStoreUpdateWithError(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(3)
	state := &unroller.UnrollState{
		VTXOOutpoint:   outpoint,
		Status:         unroller.UnrollStatusPending,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	// Fail with error message.
	state.Status = unroller.UnrollStatusFailed
	state.Error = fmt.Errorf("broadcast failed: timeout")
	state.RetryCount = 3
	err = store.UpdateUnrollState(ctx, state)
	require.NoError(t, err)

	got, err := store.GetUnrollState(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, unroller.UnrollStatusFailed, got.Status)
	require.Equal(t, 3, got.RetryCount)
	require.NotNil(t, got.Error)
	require.Contains(t, got.Error.Error(), "broadcast failed")
}

func TestUnrollStoreListActiveUnrolls(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	// Create 3 unrolls: pending, complete, broadcasting.
	states := []*unroller.UnrollState{
		{
			VTXOOutpoint:   randomOutpoint(10),
			Status:         unroller.UnrollStatusPending,
			BroadcastTxids: make(map[chainhash.Hash]bool),
			ConfirmedTxids: make(
				map[chainhash.Hash]unroller.ConfirmationInfo,
			),
		},
		{
			VTXOOutpoint:   randomOutpoint(11),
			Status:         unroller.UnrollStatusComplete,
			BroadcastTxids: make(map[chainhash.Hash]bool),
			ConfirmedTxids: make(
				map[chainhash.Hash]unroller.ConfirmationInfo,
			),
		},
		{
			VTXOOutpoint:   randomOutpoint(12),
			Status:         unroller.UnrollStatusBroadcasting,
			BroadcastTxids: make(map[chainhash.Hash]bool),
			ConfirmedTxids: make(
				map[chainhash.Hash]unroller.ConfirmationInfo,
			),
		},
	}

	for _, s := range states {
		err := store.SaveUnrollState(ctx, s)
		require.NoError(t, err)
	}

	// List active — should exclude complete.
	active, err := store.ListActiveUnrolls(ctx)
	require.NoError(t, err)
	require.Len(t, active, 2)

	// Verify the active ones are pending and broadcasting.
	activeOutpoints := make(map[string]bool)
	for _, a := range active {
		activeOutpoints[a.VTXOOutpoint.String()] = true
	}
	require.True(t, activeOutpoints[states[0].VTXOOutpoint.String()])
	require.True(t, activeOutpoints[states[2].VTXOOutpoint.String()])
}

func TestUnrollStoreListActiveExcludesFailed(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	// Create a failed unroll.
	state := &unroller.UnrollState{
		VTXOOutpoint:   randomOutpoint(20),
		Status:         unroller.UnrollStatusFailed,
		Error:          fmt.Errorf("permanent failure"),
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	active, err := store.ListActiveUnrolls(ctx)
	require.NoError(t, err)
	require.Len(t, active, 0)
}

func TestUnrollStoreDelete(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(30)
	state := &unroller.UnrollState{
		VTXOOutpoint:   outpoint,
		Status:         unroller.UnrollStatusComplete,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	// Verify it exists.
	_, err = store.GetUnrollState(ctx, outpoint)
	require.NoError(t, err)

	// Delete.
	err = store.DeleteUnrollState(ctx, outpoint)
	require.NoError(t, err)

	// Verify it's gone.
	_, err = store.GetUnrollState(ctx, outpoint)
	require.Error(t, err)
}

func TestUnrollStoreGetNotFound(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(99)
	_, err := store.GetUnrollState(ctx, outpoint)
	require.Error(t, err)
}

func TestUnrollStoreUpdateNotFound(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	state := &unroller.UnrollState{
		VTXOOutpoint:   randomOutpoint(99),
		Status:         unroller.UnrollStatusPending,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	// sqlc's UpdateUnroll is :exec, so it won't return an error
	// for non-existent rows (no rows affected is not an error).
	// This is different from the old hand-written SQL which checked
	// RowsAffected.
	err := store.UpdateUnrollState(ctx, state)
	require.NoError(t, err)
}

func TestUnrollStoreCSVAwaitingPersistence(t *testing.T) {
	t.Parallel()

	store := newTestUnrollStore(t)
	ctx := t.Context()

	outpoint := randomOutpoint(40)
	state := &unroller.UnrollState{
		VTXOOutpoint:      outpoint,
		Status:            unroller.UnrollStatusAwaitingCSV,
		CurrentLevel:      2,
		LeafConfirmHeight: 500,
		BroadcastTxids:    make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(
			map[chainhash.Hash]unroller.ConfirmationInfo,
		),
	}

	err := store.SaveUnrollState(ctx, state)
	require.NoError(t, err)

	got, err := store.GetUnrollState(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, unroller.UnrollStatusAwaitingCSV, got.Status)
	require.Equal(t, 2, got.CurrentLevel)
	require.Equal(t, int32(500), got.LeafConfirmHeight)

	// Should appear in active list.
	active, err := store.ListActiveUnrolls(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, outpoint, active[0].VTXOOutpoint)
}
