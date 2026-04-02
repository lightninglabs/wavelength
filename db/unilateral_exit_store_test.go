package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newUnilateralExitStoreForTest creates a unilateral-exit store backed by a
// fresh test database.
func newUnilateralExitStoreForTest(
	t *testing.T) *UnilateralExitPersistenceStore {

	t.Helper()

	db := NewTestDB(t)

	exitDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) UnilateralExitStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewUnilateralExitPersistenceStore(
		exitDB, clock.NewDefaultClock(),
	)
}

// TestUnilateralExitStoreListNonTerminalJobs verifies that restore queries only
// return non-terminal manager-facing job rows.
func TestUnilateralExitStoreListNonTerminalJobs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUnilateralExitStoreForTest(t)

	pendingTarget := wire.OutPoint{
		Hash:  chainhash.Hash{0x11, 0x01},
		Index: 1,
	}
	completedTarget := wire.OutPoint{
		Hash:  chainhash.Hash{0x22, 0x02},
		Index: 2,
	}
	failedTarget := wire.OutPoint{
		Hash:  chainhash.Hash{0x33, 0x03},
		Index: 3,
	}

	err := store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: pendingTarget,
		ActorID:        "job-pending",
		Status:         UnilateralExitJobStatusMaterializing,
		Trigger:        UnilateralExitJobTriggerManual,
		CreatedAt:      time.Unix(10, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: completedTarget,
		ActorID:        "job-completed",
		Status:         UnilateralExitJobStatusCompleted,
		Trigger:        UnilateralExitJobTriggerRestart,
		CreatedAt:      time.Unix(20, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: failedTarget,
		ActorID:        "job-failed",
		Status:         UnilateralExitJobStatusFailed,
		Trigger:        UnilateralExitJobTriggerCriticalExpiry,
		LastError:      "boom",
		CreatedAt:      time.Unix(30, 0),
	})
	require.NoError(t, err)

	jobs, err := store.ListNonTerminalJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, pendingTarget, jobs[0].TargetOutpoint)
	require.Equal(t, "job-pending", jobs[0].ActorID)
	require.Equal(t, UnilateralExitJobStatusMaterializing,
		jobs[0].Status)
}
