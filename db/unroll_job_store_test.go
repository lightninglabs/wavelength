package db

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func newUnrollJobStoreForTest(t *testing.T) *UnrollJobPersistenceStore {
	t.Helper()

	db := NewTestDB(t)
	jobDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) UnrollJobStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewUnrollJobPersistenceStore(jobDB, clock.NewDefaultClock())
}

func TestUnrollJobStoreListNonTerminalJobs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUnrollJobStoreForTest(t)

	pendingTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x01,
		},
		Index: 1,
	}
	completedTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x22,
			0x02,
		},
		Index: 2,
	}
	failedTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x33,
			0x03,
		},
		Index: 3,
	}

	err := store.UpsertJob(ctx, UnrollJobRecord{
		TargetOutpoint: pendingTarget,
		State:          "materializing",
		Trigger:        "manual",
		PlannerState:   []byte{0x01},
		CreatedAt:      time.Unix(10, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnrollJobRecord{
		TargetOutpoint: completedTarget,
		State:          "completed",
		Trigger:        "restart",
		PlannerState:   []byte{0x02},
		CreatedAt:      time.Unix(20, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnrollJobRecord{
		TargetOutpoint: failedTarget,
		State:          "failed",
		Trigger:        "critical_expiry",
		PlannerState:   []byte{0x03},
		FailReason:     "boom",
		CreatedAt:      time.Unix(30, 0),
	})
	require.NoError(t, err)

	jobs, err := store.ListNonTerminalJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, pendingTarget, jobs[0].TargetOutpoint)
	require.Equal(t, "materializing", jobs[0].State)
}

func TestUnrollJobStoreUpsertPersistsNamedArtifacts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUnrollJobStoreForTest(t)
	target := wire.OutPoint{
		Hash: chainhash.Hash{
			0x44,
			0x04,
		},
		Index: 4,
	}
	sweepTxid := bytes.Repeat([]byte{0xAB}, chainhash.HashSize)
	sweepTx := []byte{0x02, 0x00, 0x00, 0x00}

	err := store.UpsertJob(ctx, UnrollJobRecord{
		TargetOutpoint: target,
		State:          "sweep_confirmation",
		Trigger:        "manual",
		BestHeight:     111,
		PlannerState:   []byte{0x01},
		SweepTx:        sweepTx,
		SweepTxid:      sweepTxid,
		TxProgress: []UnrollTxProgressRecord{{
			Txid:   sweepTxid,
			Role:   "sweep",
			Status: "in_flight",
		}},
		Watches: []UnrollWatchRecord{{
			WatchID: "sweep-watch",
			Role:    "sweep",
			Txid:    sweepTxid,
			Status:  "registered",
		}},
		CreatedAt: time.Unix(40, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnrollJobRecord{
		TargetOutpoint: target,
		State:          "completed",
		Trigger:        "manual",
		BestHeight:     112,
		PlannerState:   []byte{0x02},
		SweepTx:        sweepTx,
		SweepTxid:      sweepTxid,
		TxProgress: []UnrollTxProgressRecord{{
			Txid:   sweepTxid,
			Role:   "sweep",
			Status: "confirmed",
		}},
		Watches: []UnrollWatchRecord{{
			WatchID: "sweep-watch",
			Role:    "sweep",
			Txid:    sweepTxid,
			Status:  "confirmed",
		}},
		CreatedAt: time.Unix(40, 0),
	})
	require.NoError(t, err)

	job, err := store.GetJob(ctx, target)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "completed", job.State)
	require.Equal(t, sweepTxid, job.SweepTxid)
	require.Equal(t, sweepTx, job.SweepTx)
	require.Len(t, job.TxProgress, 1)
	require.Equal(t, "sweep", job.TxProgress[0].Role)
	require.Len(t, job.Watches, 1)
	require.Equal(t, "sweep", job.Watches[0].Role)
}
