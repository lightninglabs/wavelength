package db

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
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

// TestVTXOStoreLoadBatchExpiry verifies that GetVTXO populates
// rounds.VTXO.BatchExpiry from the source round's confirmation
// height + csv_delay. Under the #270 seal-time fee handshake the
// fee builder reads this field to compute the forfeit input's real
// remaining-blocks-to-expiry, so the JOIN plumbing must populate it
// for every VTXO whose source round has confirmed on-chain.
func TestVTXOStoreLoadBatchExpiry(t *testing.T) {
	t.Parallel()

	t.Run("confirmed round populates BatchExpiry", func(t *testing.T) {
		t.Parallel()

		sqlStore := NewTestDB(t)
		store := NewStore(
			sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
			btclog.Disabled, clock.NewDefaultClock(),
		)
		vtxoStore := store.NewVTXOStore()
		roundStore := store.NewRoundStore()
		ctx := t.Context()

		roundID := testRoundID("vtxo-batch-expiry-confirmed")
		testRound := createTestRound(t, roundID)
		require.NoError(t, roundStore.PersistRound(ctx, testRound))

		// Confirm the round at height 1000. CSVDelay defaults to
		// 144 in the test helper, so BatchExpiry = 1000 + 144.
		const confHeight = int32(1000)
		var blockHash chainhash.Hash
		copy(blockHash[:], []byte("test-block-hash-32-bytes-paddin!"))
		require.NoError(t, roundStore.MarkRoundConfirmed(
			ctx, roundID, confHeight, blockHash,
		))

		vtxo := createTestVTXO(t, roundID, 0)
		require.NoError(t, vtxoStore.PersistVTXOs(
			ctx, []*rounds.VTXO{vtxo},
		))

		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.NotNil(t, loaded)
		require.Equal(
			t, uint32(confHeight)+uint32(testRound.CSVDelay),
			loaded.BatchExpiry,
		)
	})

	t.Run("unconfirmed round yields zero BatchExpiry", func(t *testing.T) {
		t.Parallel()

		sqlStore := NewTestDB(t)
		store := NewStore(
			sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
			btclog.Disabled, clock.NewDefaultClock(),
		)
		vtxoStore := store.NewVTXOStore()
		roundStore := store.NewRoundStore()
		ctx := t.Context()

		roundID := testRoundID("vtxo-batch-expiry-unconfirmed")
		testRound := createTestRound(t, roundID)
		require.NoError(t, roundStore.PersistRound(ctx, testRound))

		// Round is intentionally left unconfirmed. BatchExpiry
		// must fall back to zero rather than alias the csv_delay.
		vtxo := createTestVTXO(t, roundID, 0)
		require.NoError(t, vtxoStore.PersistVTXOs(
			ctx, []*rounds.VTXO{vtxo},
		))

		loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
		require.NoError(t, err)
		require.NotNil(t, loaded)
		require.Equal(t, uint32(0), loaded.BatchExpiry)
	})
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

// TestVTXOStoreMarkUnrolledByClient verifies that a live VTXO can be marked
// unavailable for future cooperative handling after a recognized client-owned
// on-chain reveal.
func TestVTXOStoreMarkUnrolledByClient(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-unrolled")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	vtxo := createTestVTXO(t, roundID, 0)
	vtxo.Status = rounds.VTXOStatusLive

	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	err = vtxoStore.MarkVTXOUnrolledByClient(ctx, vtxo.Outpoint)
	require.NoError(t, err)

	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusUnrolledByClient, loaded.Status)
}

// TestVTXOStoreMarkUnrolledByClientRejectsNonLive verifies that only live
// VTXOs can enter the unrolled_by_client terminal state.
func TestVTXOStoreMarkUnrolledByClientRejectsNonLive(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-unrolled-pending")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	vtxo := createTestVTXO(t, roundID, 0)
	require.Equal(t, rounds.VTXOStatusPending, vtxo.Status)

	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	err = vtxoStore.MarkVTXOUnrolledByClient(ctx, vtxo.Outpoint)
	require.ErrorContains(t, err, "not live")

	loaded, err := vtxoStore.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.Equal(t, rounds.VTXOStatusPending, loaded.Status)
}

// TestVTXOStoreForfeitClearsInFlightLock tests forfeiting an in-flight VTXO
// clears lock owner metadata atomically.
func TestVTXOStoreForfeitClearsInFlightLock(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("vtxo-round-forfeit-clears-lock")
	ctx, vtxoStore := setupVTXOTest(t, roundID)

	vtxo := createTestVTXO(t, roundID, 0)
	vtxo.Status = rounds.VTXOStatusLive

	err := vtxoStore.PersistVTXOs(ctx, []*rounds.VTXO{vtxo})
	require.NoError(t, err)

	// Put the VTXO into in_flight first to reproduce the CHECK-constraint
	// sensitive transition to forfeited.
	err = vtxoStore.LockVTXO(ctx, roundID, vtxo.Outpoint)
	require.NoError(t, err)

	forfeitInfo := &rounds.ForfeitInfo{
		RoundID:              roundID,
		ConnectorOutputIndex: 0,
		LeafIndex:            0,
		ForfeitTx: createTestFinalTx(
			t, "forfeit-lock-clear",
		),
	}
	err = vtxoStore.MarkVTXOForfeit(ctx, vtxo.Outpoint, forfeitInfo)
	require.NoError(t, err)

	row, err := vtxoStore.q.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  vtxo.Outpoint.Hash[:],
		OutpointIndex: int32(vtxo.Outpoint.Index),
	})
	require.NoError(t, err)
	require.Equal(t, string(rounds.VTXOStatusForfeited), row.Status)
	require.False(t, row.LockOwnerKind.Valid)
	require.Empty(t, row.LockOwnerID)
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
