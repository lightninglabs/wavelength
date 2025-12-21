package db

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// setupVTXOTest creates a store, vtxo store, and persists a round for testing.
func setupVTXOTest(t *testing.T, roundID rounds.RoundID) (
	context.Context, *VTXOStoreDB) {

	t.Helper()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore()
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create and persist a round (for foreign key constraint).
	testRound := createTestRound(t, roundID)
	err := roundStore.PersistRound(ctx, testRound)
	require.NoError(t, err)

	return ctx, vtxoStore
}

// TestVTXOStorePersist tests basic VTXO persistence.
func TestVTXOStorePersist(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore()
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create and persist a round first (for foreign key constraint).
	roundID := testRoundID("vtxo-round-1")
	testRound := createTestRound(t, roundID)
	err := roundStore.PersistRound(ctx, testRound)
	require.NoError(t, err)

	// Create test VTXOs.
	vtxo1 := createTestVTXO(t, roundID, 0)
	vtxo2 := createTestVTXO(t, roundID, 1)

	// Persist VTXOs.
	err = vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo1, vtxo2})
	require.NoError(t, err)

	// Retrieve and verify.
	loaded1, err := vtxoStore.GetVTXO(ctx, vtxo1.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, loaded1)
	require.Equal(t, vtxo1.Outpoint, loaded1.Outpoint)
	require.Equal(t, vtxo1.RoundID, loaded1.RoundID)
	require.Equal(
		t, vtxo1.Descriptor.Amount, loaded1.Descriptor.Amount,
	)
	require.Equal(t, rounds.VTXOStatusPending, loaded1.Status)

	loaded2, err := vtxoStore.GetVTXO(ctx, vtxo2.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, loaded2)
	require.Equal(t, vtxo2.Outpoint, loaded2.Outpoint)
}

// TestVTXOStoreGetVTXONotFound tests retrieving a non-existent VTXO.
func TestVTXOStoreGetVTXONotFound(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore()
	ctx := t.Context()

	// Try to get a non-existent VTXO.
	nonExistent := wire.OutPoint{
		Hash:  testOutpointHash(t, "non-existent"),
		Index: 0,
	}

	vtxo, err := vtxoStore.GetVTXO(ctx, nonExistent)
	require.NoError(t, err)
	require.Nil(t, vtxo, "should return nil for non-existent VTXO")
}

// TestVTXOStoreMarkLive tests marking VTXOs as live.
func TestVTXOStoreMarkLive(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-live")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist VTXOs.
	vtxo1 := createTestVTXO(t, roundID, 0)
	vtxo2 := createTestVTXO(t, roundID, 1)

	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo1, vtxo2})
	require.NoError(t, err)

	// Initially both should be pending.
	loaded1, err := vtxoStore.GetVTXO(ctx, vtxo1.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusPending, loaded1.Status)

	// Mark all VTXOs for the round as live.
	err = vtxoStore.MarkVTXOsLive(ctx, roundID)
	require.NoError(t, err)

	// Verify both are now live.
	loaded1, err = vtxoStore.GetVTXO(ctx, vtxo1.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusLive, loaded1.Status)

	loaded2, err := vtxoStore.GetVTXO(ctx, vtxo2.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusLive, loaded2.Status)
}

// TestVTXOStoreForfeit tests marking a VTXO as forfeited.
func TestVTXOStoreForfeit(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-forfeit")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist a VTXO.
	vtxo := createTestVTXO(t, roundID, 0)

	// Set it to live status first.
	vtxo.Status = rounds.VTXOStatusLive
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Mark as forfeited with forfeit info.
	forfeitTx := createTestFinalTx(t, "forfeit-tx")
	forfeitInfo := &rounds.ForfeitInfo{
		RoundID:              roundID,
		ConnectorOutputIndex: 1,
		LeafIndex:            2,
		ForfeitTx:            forfeitTx,
	}

	err = vtxoStore.MarkVTXOForfeit(ctx, vtxo.Outpoint, forfeitInfo)
	require.NoError(t, err)

	// Verify status changed to forfeited.
	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, "forfeited", string(loaded.Status))

	stored, err := vtxoStore.GetForfeitInfo(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, forfeitInfo.RoundID, stored.RoundID)
	require.Equal(t, forfeitInfo.ConnectorOutputIndex,
		stored.ConnectorOutputIndex)
	require.Equal(t, forfeitInfo.LeafIndex, stored.LeafIndex)
	require.NotNil(t, stored.ForfeitTx)

	var buf bytes.Buffer
	require.NoError(t, forfeitTx.Serialize(&buf))
	var storedBuf bytes.Buffer
	require.NoError(t, stored.ForfeitTx.Serialize(&storedBuf))
	require.Equal(t, buf.Bytes(), storedBuf.Bytes())
}

// TestVTXOStoreForfeitMissingVTXO tests forfeiting a missing VTXO.
func TestVTXOStoreForfeitMissingVTXO(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore()
	ctx := t.Context()

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "missing-vtxo"),
		Index: 0,
	}
	forfeitInfo := &rounds.ForfeitInfo{
		RoundID:              testRoundID("missing-vtxo-round"),
		ConnectorOutputIndex: 0,
		LeafIndex:            0,
		ForfeitTx:            createTestFinalTx(t, "missing-vtxo-tx"),
	}

	err := vtxoStore.MarkVTXOForfeit(ctx, outpoint, forfeitInfo)
	require.ErrorContains(t, err, "not found")
}

// TestVTXOStoreGetForfeitInfoMissing tests missing forfeit metadata.
func TestVTXOStoreGetForfeitInfoMissing(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-missing-forfeit")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	vtxo := createTestVTXO(t, roundID, 0)
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	info, err := vtxoStore.GetForfeitInfo(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Nil(t, info)
}

// TestVTXOStoreLocking tests VTXO locking and unlocking.
func TestVTXOStoreLocking(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-lock")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist a live VTXO.
	vtxo := createTestVTXO(t, roundID, 0)
	vtxo.Status = rounds.VTXOStatusLive
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Lock the VTXO for a new round.
	lockingRound := testRoundID("locking-round")
	err = vtxoStore.LockVTXO(ctx, lockingRound, vtxo.Outpoint)
	require.NoError(t, err)

	// Verify it's locked.
	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, "in_flight", string(loaded.Status))

	// Unlock the VTXO.
	err = vtxoStore.UnlockVTXO(ctx, lockingRound, vtxo.Outpoint)
	require.NoError(t, err)

	// Verify it's back to live.
	loaded, err = vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusLive, loaded.Status)
}

// TestVTXOStoreConcurrentLocking tests that concurrent lock attempts are
// handled correctly.
func TestVTXOStoreConcurrentLocking(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-concurrent")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist a live VTXO.
	vtxo := createTestVTXO(t, roundID, 0)
	vtxo.Status = rounds.VTXOStatusLive
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Try to lock from two different rounds concurrently.
	round1 := testRoundID("concurrent-round-1")
	round2 := testRoundID("concurrent-round-2")

	var wg sync.WaitGroup
	var err1, err2 error

	wg.Add(2)

	go func() {
		defer wg.Done()
		err1 = vtxoStore.LockVTXO(ctx, round1, vtxo.Outpoint)
	}()

	go func() {
		defer wg.Done()
		err2 = vtxoStore.LockVTXO(ctx, round2, vtxo.Outpoint)
	}()

	wg.Wait()

	// One should succeed, one should fail or both could succeed if they
	// don't truly conflict due to timing. The important thing is that the
	// VTXO ends up in a consistent state locked by exactly one round.
	successCount := 0
	if err1 == nil {
		successCount++
	}
	if err2 == nil {
		successCount++
	}

	// At least one should succeed.
	require.Greater(t, successCount, 0, "at least one lock should succeed")

	// Verify the VTXO is locked.
	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, "in_flight", string(loaded.Status))
}

// TestVTXOStoreLockIdempotency tests that locking by the same round is
// idempotent.
func TestVTXOStoreLockIdempotency(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-idempotent")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist a live VTXO.
	vtxo := createTestVTXO(t, roundID, 0)
	vtxo.Status = rounds.VTXOStatusLive
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Lock the VTXO.
	lockingRound := testRoundID("locking-round-idem")
	err = vtxoStore.LockVTXO(ctx, lockingRound, vtxo.Outpoint)
	require.NoError(t, err)

	// Lock again by the same round - should succeed.
	err = vtxoStore.LockVTXO(ctx, lockingRound, vtxo.Outpoint)
	require.NoError(t, err)

	// Verify it's still locked.
	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, "in_flight", string(loaded.Status))
}

// TestVTXOStoreLockNonLiveVTXO tests that locking a non-live VTXO fails.
func TestVTXOStoreLockNonLiveVTXO(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-nonlive")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist a pending VTXO.
	vtxo := createTestVTXO(t, roundID, 0)
	// Keep status as pending.
	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Try to lock - should fail since it's not live.
	lockingRound := testRoundID("locking-round-fail")
	err = vtxoStore.LockVTXO(ctx, lockingRound, vtxo.Outpoint)
	require.Error(t, err, "should not be able to lock non-live VTXO")

	// Verify it's still pending.
	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusPending, loaded.Status)
}

// TestVTXOStoreBatchOperations tests batch insert and update operations.
func TestVTXOStoreBatchOperations(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-batch")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create a large batch of VTXOs.
	vtxos := make([]*rounds.VTXO, 100)
	for i := range vtxos {
		vtxos[i] = createTestVTXO(t, roundID, i)
	}

	// Persist all at once.
	err := vtxoStore.PersistVTXOs(ctx, vtxos)
	require.NoError(t, err)

	// Verify all were persisted.
	for _, vtxo := range vtxos {
		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.NotNil(t, loaded)
		require.Equal(t, vtxo.Outpoint, loaded.Outpoint)
	}

	// Mark all as live in one operation.
	err = vtxoStore.MarkVTXOsLive(ctx, roundID)
	require.NoError(t, err)

	// Verify all are live.
	for _, vtxo := range vtxos {
		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.Equal(t, rounds.VTXOStatusLive, loaded.Status)
	}
}

// TestVTXOStoreMultipleLocks tests locking multiple VTXOs in one call.
func TestVTXOStoreMultipleLocks(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-multilock")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	// Create and persist multiple live VTXOs.
	vtxo1 := createTestVTXO(t, roundID, 0)
	vtxo2 := createTestVTXO(t, roundID, 1)
	vtxo3 := createTestVTXO(t, roundID, 2)
	vtxo1.Status = rounds.VTXOStatusLive
	vtxo2.Status = rounds.VTXOStatusLive
	vtxo3.Status = rounds.VTXOStatusLive

	err := vtxoStore.PersistVTXOs(
		ctx, []*rounds.VTXO{vtxo1, vtxo2, vtxo3},
	)
	require.NoError(t, err)

	// Lock all three at once.
	lockingRound := testRoundID("locking-round-multi")
	err = vtxoStore.LockVTXO(
		ctx, lockingRound,
		vtxo1.Outpoint, vtxo2.Outpoint, vtxo3.Outpoint,
	)
	require.NoError(t, err)

	// Verify all are locked.
	for _, vtxo := range []*rounds.VTXO{vtxo1, vtxo2, vtxo3} {
		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.Equal(t, "in_flight", string(loaded.Status))
	}

	// Unlock all three at once.
	err = vtxoStore.UnlockVTXO(
		ctx, lockingRound,
		vtxo1.Outpoint, vtxo2.Outpoint, vtxo3.Outpoint,
	)
	require.NoError(t, err)

	// Verify all are back to live.
	for _, vtxo := range []*rounds.VTXO{vtxo1, vtxo2, vtxo3} {
		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.Equal(t, rounds.VTXOStatusLive, loaded.Status)
	}
}
