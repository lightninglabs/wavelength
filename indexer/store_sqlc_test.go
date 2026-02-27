package indexer_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestSQLCStore creates a test DB and returns both the raw store (for
// seeding test data) and the SQLCStore adapter (for exercising the
// Backend() dispatch).
func newTestSQLCStore(t *testing.T) (*db.Store, *indexer.SQLCStore) {
	t.Helper()

	sqlDB := db.NewTestDB(t)
	store := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)

	return store, indexer.NewSQLCStore(store.Queries)
}

// newTestRoundID generates a deterministic round ID from a seed byte.
func newTestRoundID(seed byte) rounds.RoundID {
	var id rounds.RoundID
	for i := range id {
		id[i] = seed
	}

	return id
}

// TestSQLCStoreListVTXOsByPkScripts verifies that the Backend() dispatch
// for ListVTXOsByPkScripts works correctly. The active backend is
// determined by the build tag (SQLite by default, PostgreSQL with
// test_postgres).
func TestSQLCStoreListVTXOsByPkScripts(t *testing.T) {
	t.Parallel()

	store, sqlcStore := newTestSQLCStore(t)
	ctx := t.Context()

	pkScript1, _ := newTestP2TRScript(t)
	pkScript2, _ := newTestP2TRScript(t)
	pkScriptOther, _ := newTestP2TRScript(t)

	roundID := newTestRoundID(0x01)
	cosignerKey, _ := newTestP2TRScript(t)

	// Insert a round so the VTXO foreign key is satisfied.
	now := time.Now().Unix()
	err := store.Queries.InsertRound(ctx, sqlc.InsertRoundParams{
		RoundID: roundID[:],
		FinalTx: []byte{0x01},
		CommitmentTxid: fmt.Sprintf(
			"%064x", roundID[:],
		),
		Status:    "pending",
		SweepKey:  cosignerKey,
		CsvDelay:  144,
		CreatedAt: now,
		UpdatedAt: now,
	})
	require.NoError(t, err)

	// Insert VTXOs for two different pk_scripts.
	for i, pk := range [][]byte{pkScript1, pkScript2} {
		err := store.Queries.InsertVTXO(ctx, sqlc.InsertVTXOParams{
			OutpointHash:  make([]byte, 32),
			OutpointIndex: int32(i),
			RoundID:       roundID[:],
			BatchOutputIndex: sql.NullInt32{
				Int32: 0, Valid: true,
			},
			Amount:      1000 * int64(i+1),
			PkScript:    pk,
			CosignerKey: cosignerKey,
			Status:      "live",
		})
		require.NoError(t, err)
	}

	// Query for both scripts — should return 2 VTXOs.
	rows, err := sqlcStore.ListVTXOsByPkScripts(
		ctx, [][]byte{pkScript1, pkScript2},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Query for a single script — should return 1 VTXO.
	rows, err = sqlcStore.ListVTXOsByPkScripts(
		ctx, [][]byte{pkScript1},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1000), rows[0].Amount)

	// Query for an unrelated script — should return empty.
	rows, err = sqlcStore.ListVTXOsByPkScripts(
		ctx, [][]byte{pkScriptOther},
	)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestSQLCStoreListRoundsByIDs verifies that the Backend() dispatch for
// ListRoundsByIDs works correctly.
func TestSQLCStoreListRoundsByIDs(t *testing.T) {
	t.Parallel()

	store, sqlcStore := newTestSQLCStore(t)
	ctx := t.Context()

	cosignerKey, _ := newTestP2TRScript(t)
	now := time.Now().Unix()

	// Insert 3 rounds.
	roundIDs := make([]rounds.RoundID, 3)
	for i := range roundIDs {
		roundIDs[i] = newTestRoundID(byte(i + 1))

		err := store.Queries.InsertRound(ctx, sqlc.InsertRoundParams{
			RoundID: roundIDs[i][:],
			FinalTx: []byte{0x01},
			CommitmentTxid: fmt.Sprintf(
				"%064x", roundIDs[i][:],
			),
			Status:    "pending",
			SweepKey:  cosignerKey,
			CsvDelay:  144,
			CreatedAt: now,
			UpdatedAt: now,
		})
		require.NoError(t, err)
	}

	// Query for all 3 — should return 3.
	rows, err := sqlcStore.ListRoundsByIDs(ctx, roundIDs)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	// Query for a subset — should return 2.
	rows, err = sqlcStore.ListRoundsByIDs(
		ctx, roundIDs[:2],
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Query for a non-existent round — should return empty.
	missing := newTestRoundID(0xff)
	rows, err = sqlcStore.ListRoundsByIDs(
		ctx, []rounds.RoundID{missing},
	)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestSQLCStoreListVTXOEventsAfterByScripts verifies that the Backend()
// dispatch for ListVTXOEventsAfterByScripts works correctly, including
// the different param struct mapping for SQLite vs PostgreSQL.
func TestSQLCStoreListVTXOEventsAfterByScripts(t *testing.T) {
	t.Parallel()

	store, sqlcStore := newTestSQLCStore(t)
	ctx := t.Context()

	pkScript1, _ := newTestP2TRScript(t)
	pkScript2, _ := newTestP2TRScript(t)
	now := time.Now()

	// Insert VTXO events for two different scripts.
	for i, pk := range [][]byte{pkScript1, pkScript2} {
		_, err := store.Queries.InsertIndexerVTXOEvent(
			ctx, sqlc.InsertIndexerVTXOEventParams{
				PkScript:      pk,
				EventType:     "created",
				OutpointHash:  make([]byte, 32),
				OutpointIndex: int32(i),
				Status:        "live",
				CreatedAt:     now.Unix(),
			},
		)
		require.NoError(t, err)
	}

	// Query for both scripts — should return 2 events.
	events, err := sqlcStore.ListVTXOEventsAfterByScripts(
		ctx, 0, [][]byte{pkScript1, pkScript2}, 10,
	)
	require.NoError(t, err)
	require.Len(t, events, 2)

	// Query with afterEventID filter — should return subset.
	firstID := events[0].EventID
	events, err = sqlcStore.ListVTXOEventsAfterByScripts(
		ctx, firstID, [][]byte{pkScript1, pkScript2}, 10,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)

	// Query for a single script — should return 1.
	events, err = sqlcStore.ListVTXOEventsAfterByScripts(
		ctx, 0, [][]byte{pkScript1}, 10,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "created", events[0].EventType)
	require.Equal(t, "live", events[0].Status)

	// Query with limit — should respect the limit.
	events, err = sqlcStore.ListVTXOEventsAfterByScripts(
		ctx, 0, [][]byte{pkScript1, pkScript2}, 1,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)
}
